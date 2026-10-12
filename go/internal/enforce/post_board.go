package enforce

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"purser-hook/internal/lease"
)

type boardJob struct{ job, index map[string]any }

var RefreshHandAppliedJob func(stateRoot, jobID string)

// PostProjectJobBoard reads the same durable state as codex._project_job_board_details.
func PostProjectJobBoard(root, projectKey string, now time.Time) (string, []string) {
	defer func() {}()
	jobs := []boardJob{}
	paths, _ := filepath.Glob(filepath.Join(root, "async_jobs", "*.json"))
	idPattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, path := range paths {
		index := readBoardJSON(path)
		if index == nil || index["project_key"] != projectKey {
			continue
		}
		id, ok := index["jobId"].(string)
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		if RefreshHandAppliedJob != nil {
			RefreshHandAppliedJob(root, id)
		}
		job := readBoardJSON(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"))
		if job != nil && job["jobId"] == id && job["call"] == index["call"] {
			if label, _ := job["label"].(string); job["state"] == "QUEUED" && strings.TrimSpace(label) == "" {
				label, _ = job["taskId"].(string)
				if strings.TrimSpace(label) == "" {
					label, _ = job["task_id"].(string)
				}
				if strings.TrimSpace(label) == "" {
					label = id
				}
				job["label"] = label
			}
			jobs = append(jobs, boardJob{job, index})
		}
	}
	all := jobs
	queuedBehind := map[string]string{}
	if state := readBoardJSON(filepath.Join(root, "projects", projectKey, "scope_leases.json")); state != nil {
		owners, _ := state["owners"].(map[string]any)
		labels := map[string]string{}
		for _, item := range all {
			if id, ok := item.job["jobId"].(string); ok {
				if label, ok := item.job["label"].(string); ok {
					labels[id] = label
				}
			}
		}
		for _, queuedRaw := range owners {
			queued, ok := queuedRaw.(map[string]any)
			if !ok || queued["state"] != "QUEUED" {
				continue
			}
			qid, ok := queued["job_id"].(string)
			if !ok {
				continue
			}
			for _, activeRaw := range owners {
				active, ok := activeRaw.(map[string]any)
				if !ok || (active["state"] != "ACTIVE" &&
					!(active["state"] == "QUEUED" && boardNumber(active["queued_at"]) < boardNumber(queued["queued_at"]))) {
					continue
				}
				qs, _ := queued["scope"].([]any)
				as, _ := active["scope"].([]any)
				if !lease.ScopesOverlap(qs, as) {
					continue
				}
				if label := labels[fmt.Sprint(active["job_id"])]; strings.TrimSpace(label) != "" {
					queuedBehind[qid] = label
					break
				}
				queuedBehind[qid] = fmt.Sprint(active["task_id"])
				break
			}
		}
	}
	newest := map[string]boardJob{}
	newestOrder := []string{}
	for _, item := range jobs {
		j := item.job
		if j["state"] == "SUPERSEDED" {
			continue
		}
		key, _ := j["threadId"].(string)
		if key == "" {
			key = fmt.Sprint(j["jobId"])
		}
		old, exists := newest[key]
		if !exists {
			newestOrder = append(newestOrder, key)
		}
		if !exists || boardNewer(j, old.job) {
			newest[key] = item
		}
	}
	inProgress := []boardJob{}
	finished := []boardJob{}
	ids := []string{}
	for _, key := range newestOrder {
		item := newest[key]
		j := item.job
		label, ok := j["label"].(string)
		if !ok || strings.TrimSpace(label) == "" {
			continue
		}
		stamp, hasStamp := boardFinishedStamp(j)
		state := j["state"]
		if state != "QUEUED" && state != "RUNNING" {
			count, exists := j["boardDisplayCount"]
			n, isInt := count.(json.Number)
			hiddenInt := false
			if isInt {
				v, e := n.Int64()
				hiddenInt = e == nil && v >= 2
			}
			if !exists || hiddenInt {
				continue
			}
			if hasStamp && float64(now.UnixNano())/1e9-stamp > 3600 {
				continue
			}
		}
		if state == "QUEUED" || state == "RUNNING" {
			inProgress = append(inProgress, item)
		} else {
			finished = append(finished, item)
		}
		ids = append(ids, fmt.Sprint(j["jobId"]))
	}
	if len(inProgress) == 0 {
		visible := false
		for _, item := range finished {
			if boardDisplayZero(item.job["boardDisplayCount"]) {
				visible = true
			}
		}
		if !visible {
			return "", nil
		}
	}
	sort.Slice(inProgress, func(i, j int) bool {
		a, b := inProgress[i].job, inProgress[j].job
		ar, br := a["state"] == "RUNNING", b["state"] == "RUNNING"
		if ar != br {
			return ar
		}
		return boardNumber(a["acceptedAt"]) > boardNumber(b["acceptedAt"])
	})
	sort.Slice(finished, func(i, j int) bool { return boardFinish(finished[i].job) > boardFinish(finished[j].job) })
	ordered := append(inProgress, finished...)
	type renderedBoardRow struct {
		line     string
		rank     int
		accepted float64
		finished float64
	}
	rendered := []renderedBoardRow{}
	for _, item := range ordered {
		j, idx := item.job, item.index
		id, _ := j["jobId"].(string)
		label := strings.NewReplacer("|", "\\|", "\r", " ", "\n", " ").Replace(fmt.Sprint(j["label"]))
		state := map[string]string{"QUEUED": "queued", "RUNNING": "coding", "SUCCEEDED": "done", "FAILED": "failed", "TIMED_OUT": "failed", "CANCELLED": "cancelled", "UNKNOWN_OR_IN_PROGRESS": "coding"}[fmt.Sprint(j["state"])]
		if state == "" {
			state = "failed"
		}
		if state == "queued" && queuedBehind[id] != "" {
			state = "queued behind " + queuedBehind[id]
		}
		if j["state"] == "RUNNING" && j["errorCode"] == "STALLED" {
			state = "stalled"
		}
		bundle := readBoardJSON(filepath.Join(root, "bundle_artifacts", fmt.Sprint(idx["call_key"])+".json"))
		review := "—"
		if bundle != nil {
			switch {
			case truthy(bundle["reviewer1_result"]):
				if bundle["reviewer1_result"] == "REVIEWER1_UNAVAILABLE" {
					review = "failed"
				} else {
					review = "reviewed"
				}
			case truthy(bundle["reviewer1_skipped_reason"]):
				if bundle["reviewer1_skipped_reason"] == "REVIEWER1_UNAVAILABLE" {
					review = "failed"
				} else {
					review = "skipped"
				}
			case contains([]any{"operation", "plan"}, first(j["phase"], bundle["phase"])):
				review = "skipped"
			case bundle["claude_review_mode"] == "FULL" || bundle["claude_review_mode"] == "AUDIT":
				review = "failed"
			case j["state"] == "FAILED" || j["state"] == "TIMED_OUT":
				review = "failed"
			}
		}
		changed, ok := j["changed_paths"].([]any)
		if !ok && bundle != nil {
			changed, ok = bundle["changed_paths"].([]any)
		}
		apply := "—"
		applied := j["applyState"] == "applied" || threadApplyCovers(j, changed, all)
		if applied {
			apply = "applied"
		} else if j["applyState"] == "conflict" {
			apply = "conflict"
		} else if len(bundle) > 0 && (contains([]any{"operation", "plan"}, first(j["phase"], bundle["phase"])) || bundle["apply"] != "codex_apply") {
			apply = "n/a"
		} else if (j["phase"] == "operation" || j["phase"] == "plan") && bundle == nil {
			apply = "n/a"
		}
		if apply == "—" && j["state"] != "QUEUED" && j["state"] != "RUNNING" && j["state"] != "UNKNOWN_OR_IN_PROGRESS" && ok && len(changed) == 0 {
			apply = "no change"
		}
		if j["state"] == "SUCCEEDED" {
			switch {
			case len(bundle) == 0:
				state = "coded · fetching result"
				if apply == "applied" {
					state = "done"
				}
			case contains([]any{"applied", "n/a", "no change"}, apply):
				state = "done"
			case truthy(j["acceptance"]):
				state = "accepted"
			default:
				state = "in review"
			}
		}
		if applied {
			state = "done"
		}
		if j["discarded"] == true {
			state, review, apply = "discarded", "—", "—"
		}
		eta := "—"
		if state == "coding" || state == "stalled" {
			elapsed := (float64(now.UnixNano())/1e9 - boardNumber(first(j["startedAt"], j["acceptedAt"]))) / 60
			expected, ok := j["etaMinutes"].(json.Number)
			minutes := int64(0)
			if ok {
				parsed, parseErr := expected.Int64()
				minutes, ok = parsed, parseErr == nil
			}
			if !ok {
				minutes = map[string]int64{"mechanical": 3, "subtle": 5, "normal": 5, "consequential": 8, "escalated": 12, "operation": 3, "plan_standard": 8}[fmt.Sprint(j["route"])]
			}
			if hasBoardNumber(first(j["startedAt"], j["acceptedAt"])) && minutes > 0 {
				if elapsed < 0 {
					elapsed = 0
				}
				if elapsed <= 2*float64(minutes) {
					eta = fmt.Sprintf("~%d min", maxInt(0, int(math.Ceil(float64(minutes)-elapsed))))
				}
			}
		}
		model, effort := j["model"], j["effort"]
		modelLabel := "—"
		if m, mok := model.(string); mok {
			if e, eok := effort.(string); eok {
				modelLabel = strings.NewReplacer("|", "\\|").Replace(m + " (" + e + ")")
			}
		}
		rank := 3
		switch {
		case j["state"] == "RUNNING":
			rank = 0
		case j["state"] == "SUCCEEDED" && state != "done":
			rank = 1
		case j["state"] == "QUEUED":
			rank = 2
		}
		rendered = append(rendered, renderedBoardRow{
			line: fmt.Sprintf("> | %s | %s | %s | %s | %s | %s |", label, modelLabel, state, eta, review, apply),
			rank: rank, accepted: boardNumber(j["acceptedAt"]), finished: boardFinish(j),
		})
	}
	sort.SliceStable(rendered, func(i, j int) bool {
		a, b := rendered[i], rendered[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.rank == 0 || a.rank == 2 {
			return a.accepted > b.accepted
		}
		return a.finished > b.finished
	})
	rows := []string{"> | ⚓ Task | Model | State | ETA | Review | Apply |", "> | --- | --- | --- | --- | --- | --- |"}
	for _, row := range rendered {
		rows = append(rows, row.line)
	}
	return "Purser jobs:\n" + strings.Join(rows, "\n"), ids
}

func boardDisplayZero(value any) bool {
	if value == false {
		return true
	}
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := number.Float64()
	return err == nil && parsed == 0
}

func refreshCachedBoard(response map[string]any, board string) {
	output := mapValue(response["hookSpecificOutput"])
	if output == nil {
		return
	}
	context := stringValue(output["additionalContext"])
	if i := strings.Index(context, "Purser jobs:\n"); i >= 0 {
		before, after := context[:i], context[i+len("Purser jobs:\n"):]
		lines := strings.Split(after, "\n")
		for len(lines) > 0 && strings.HasPrefix(lines[0], "> |") {
			lines = lines[1:]
		}
		context = strings.TrimSpace(before + strings.Join(lines, "\n"))
	}
	if board != "" {
		if context != "" {
			context += "\n"
		}
		context += board
	}
	if context == "" {
		delete(output, "additionalContext")
	} else {
		output["additionalContext"] = context
	}
}

func readBoardJSON(path string) map[string]any {
	b, e := os.ReadFile(path)
	if e != nil || !utf8.Valid(b) {
		return nil
	}
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return nil
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil
	}
	return v
}
func boardNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		x, _ := n.Float64()
		return x
	}
	return 0
}
func boardFinish(j map[string]any) float64 {
	stamp, _ := boardFinishedStamp(j)
	return stamp
}
func boardFinishedStamp(j map[string]any) (float64, bool) {
	if j["applyState"] == "applied" && hasBoardNumber(j["appliedAt"]) {
		return boardNumber(j["appliedAt"]), true
	}
	return boardNumber(j["finishedAt"]), hasBoardNumber(j["finishedAt"])
}
func hasBoardNumber(v any) bool {
	switch v.(type) {
	case float64, int, json.Number:
		return true
	}
	return false
}
func boardNewer(a, b map[string]any) bool {
	x, y := boardNumber(a["acceptedAt"]), boardNumber(b["acceptedAt"])
	if x != y {
		return x > y
	}
	return fmt.Sprint(a["jobId"]) > fmt.Sprint(b["jobId"])
}
func truthy(v any) bool {
	if v == nil || v == false || v == "" {
		return false
	}
	return true
}
func contains(values []any, value any) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func first(a, b any) any {
	if a != nil {
		return a
	}
	return b
}
func threadApplyCovers(job map[string]any, changed []any, all []boardJob) bool {
	thread, ok := job["threadId"].(string)
	if !ok || thread == "" || len(changed) == 0 {
		return false
	}
	for _, v := range changed {
		if _, ok := v.(string); !ok {
			return false
		}
	}
	for _, other := range all {
		if other.job["threadId"] != thread || other.job["applyState"] != "applied" {
			continue
		}
		result, _ := other.job["applyResult"].(map[string]any)
		if result["applied"] != true {
			continue
		}
		paths, ok := result["paths"].([]any)
		if !ok {
			continue
		}
		covers := true
		for _, p := range changed {
			if !contains(paths, p) {
				covers = false
			}
		}
		if covers {
			return true
		}
	}
	return false
}

// PostDispatchReceipt renders the admitted dispatch receipt from recorded routing data.
func PostDispatchReceipt(pending map[string]any, eta *int) string {
	label, l := pending["label"].(string)
	model, m := pending["model"].(string)
	effort, e := pending["effort"].(string)
	route, r := pending["route"].(string)
	if !l || !m || !e || !r || label == "" || model == "" || effort == "" || route == "" {
		return ""
	}
	continuity := "New session"
	if id, ok := pending["prior_thread_id"].(string); ok && id != "" {
		continuity = "Continuing session"
	}
	fields := []string{continuity + " · model: " + model + " (" + effort + ") · task: " + label}
	if eta != nil {
		fields = append(fields, fmt.Sprintf("estimated completion: %d min", *eta))
	}
	return "> ⚓ **Codex: " + strings.Join(fields, " · ") + "**"
}

// PostApplyGuidance is the exact codex_apply instruction appended after worktree review.
func PostApplyGuidance(prior, jobID string) string {
	sentence := fmt.Sprintf("After accepting the review, call codex_apply with jobId %s in the same turn; a conflict result lists the paths and keeps the worktree.", jobID)
	if prior != "" {
		return prior + "\n" + sentence
	}
	return sentence
}

func appendPostApplyGuidance(response map[string]any, jobID string) {
	if response == nil {
		return
	}
	output := mapValue(response["hookSpecificOutput"])
	if output == nil {
		output = map[string]any{}
		response["hookSpecificOutput"] = output
	}
	if _, ok := output["hookEventName"]; !ok {
		output["hookEventName"] = "PostToolUse"
	}
	guidance := PostApplyGuidance("", jobID)
	if prior := stringValue(output["additionalContext"]); prior != "" {
		output["additionalContext"] = prior + "\n" + guidance
	} else {
		output["additionalContext"] = guidance
	}
}

// PostReceiptAdvice mirrors compaction_advisor._receipt_advice.
func PostReceiptAdvice(receipt map[string]any, now time.Time) string {
	parts := []string{"model " + fmt.Sprint(receipt["model"]), "effort " + fmt.Sprint(receipt["effort"])}
	if receipt["gear"] == "up" {
		parts = append(parts, "gear up")
	}
	current := true
	if captured, ok := receipt["snapshot_at"].(string); ok {
		if parsed, err := parseBoardTime(captured); err == nil && !parsed.IsZero() && now.Sub(parsed) > time.Hour {
			current = false
		}
	}
	if reset, ok := receipt["5h_resets_at"].(string); ok {
		if parsed, err := parseBoardTime(reset); err == nil && !parsed.IsZero() && !parsed.After(now) {
			current = false
		}
	}
	if !current {
		return "The quota snapshot is stale. Write one line in the user's language as `> ⚓ **Codex done: ...**`, in the user's language, showing " + strings.Join(parts, ", ") + "."
	}
	for _, entry := range []struct {
		key, label, used string
		weekly           bool
	}{{"5h_resets_at", "5h", "5h_used_pct", false}, {"7d_resets_at", "7d", "7d_used_pct", true}} {
		if value, ok := receipt[entry.used]; ok {
			parts = append(parts, entry.label+" used "+fmt.Sprint(value)+"%")
		}
		raw, ok := receipt[entry.key].(string)
		if !ok {
			continue
		}
		parsed, err := parseBoardTime(raw)
		if err != nil || parsed.IsZero() {
			continue
		}
		mins := int(now.Sub(parsed) * -1 / time.Minute)
		if mins < 0 {
			mins = 0
		}
		if entry.weekly && mins >= 24*60 {
			parts = append(parts, fmt.Sprintf("time until 7d reset %dd %dh", mins/(24*60), mins%(24*60)/60))
		} else {
			parts = append(parts, fmt.Sprintf("time until %s reset %dh %dm", entry.label, mins/60, mins%60))
		}
	}
	return "Write one line in the user's language as `> ⚓ **Codex done: ...**`, showing " + strings.Join(parts, ", ") + "."
}

func parseBoardTime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02T15:04:05", value)
}
