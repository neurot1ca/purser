package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const stopSuffix = "\n\nYour previous reply is already visible to the user: write only the lines requested above, without repeating any of its text."

var anchorLine = regexp.MustCompile(`(?m)^(?:\*\*⚓ .+\*\*|> ⚓ \*\*.+\*\*|\*\*> ⚓ \*\*.+\*\*\*\*)$`)

var stopTaskFinalizer func(root, session, transcriptPath string)

// SetStopTaskFinalizer registers the enforcement package's observational Stop finalizer.
func SetStopTaskFinalizer(finalize func(root, session, transcriptPath string)) {
	stopTaskFinalizer = finalize
}

// Stop handles context lifecycle notices and the registered enforcement Stop finalizer.
func Stop(event map[string]any, started time.Time) []byte {
	stopTurn(event, started)
	repository := projectRoot(event)
	session := str(event["session_id"])
	if event["stop_hook_active"] != true && session != "" && stopTaskFinalizer != nil {
		defer func() {
			defer func() { _ = recover() }() // Finalization is observational and must not affect Stop.
			stopTaskFinalizer(stateRoot(), session, str(event["transcript_path"]))
		}()
	}
	if repository == "" || session == "" {
		return nil
	}
	path := filepath.Join(stateRoot(), "projects", repositoryProjectKey(repository), "telemetry", "compaction_advisor.json")
	state := readAdvisorState(path, session)
	for _, field := range []string{"notice_pending", "maintenance_pending", "receipt_pending", "dispatch_pending", "version_pending", "leader_pending", "fallback_pending"} {
		pending, ok := state[field].(map[string]any)
		if !ok || !validStopPending(field, pending) {
			state[field] = nil
		}
	}
	records := readTranscriptRecords(str(event["transcript_path"]))
	latestUser := -1
	for i, record := range records {
		if genuineTranscriptUser(record) {
			latestUser = i
		}
	}
	var turn []string
	for _, record := range records[latestUser+1:] {
		if transcriptRecordType(record) == "assistant" {
			if text := transcriptText(record); text != "" {
				turn = append(turn, text)
			}
		}
	}
	message := ""
	if last, exists := event["last_assistant_message"]; exists {
		message = str(last)
	} else if len(turn) > 0 {
		message = turn[len(turn)-1]
	}
	joined := strings.Join(turn, "\n")
	if message != "" && !strings.Contains(joined, message) {
		joined += "\n" + message
	}
	changed := false
	clear := func(field string) { state[field] = nil; changed = true }
	if notice, ok := state["notice_pending"].(map[string]any); ok && validNoticePending(notice) {
		for _, line := range strings.Split(joined, "\n") {
			if strings.HasPrefix(line, "> ⚓") {
				clear("notice_pending")
				break
			}
		}
	}
	if pending, ok := state["maintenance_pending"].(map[string]any); ok {
		if evidence, ok := pending["evidence"].([]any); ok {
			for _, item := range evidence {
				if s := str(item); s != "" && strings.Contains(joined, s) {
					clear("maintenance_pending")
					confirmMaintenanceNotice(str(pending["notice_file"]))
					break
				}
			}
		}
	}
	if dispatch, ok := state["dispatch_pending"].(map[string]any); ok {
		lines, valid := dispatch["lines"].([]any)
		board, hasBoard := dispatch["board"].(string)
		if valid {
			all := true
			for _, line := range lines {
				if str(line) == "" || !strings.Contains(joined, str(line)) {
					all = false
				}
			}
			if all && (!hasBoard || strings.Contains(joined, board)) {
				// Board display counts are updated when the visible board is confirmed.
				if hasBoard {
					markBoardDisplayed(dispatch)
				}
				clear("dispatch_pending")
			}
		}
	}
	if pending, ok := state["version_pending"].(map[string]any); ok && str(pending["version"]) != "" && strings.Contains(joined, "⚓") && strings.Contains(joined, str(pending["version"])) {
		clear("version_pending")
		_ = writeJSONAtomically(filepath.Join(stateRoot(), "plugin-version.json"), map[string]any{"announced_version": pending["version"]})
	}
	if pending, ok := state["leader_pending"].(map[string]any); ok && str(pending["model"]) != "" {
		for _, line := range strings.Split(joined, "\n") {
			if strings.Contains(line, "⚓") && strings.Contains(line, str(pending["model"])) {
				clear("leader_pending")
				markLeaderAnnounced(str(pending["model"]))
				break
			}
		}
	}
	if pending, ok := state["fallback_pending"].(map[string]any); ok && len(anchorLine.FindAllString(joined, -1)) == 1 && strings.Contains(anchorLine.FindString(joined), str(pending["model"])) {
		markFallbackShown(str(pending["project"]), session)
		clear("fallback_pending")
	}
	if event["stop_hook_active"] == true {
		if changed {
			_ = writeAdvisorState(path, state)
		}
		return nil
	}
	var retryReason string
	if latestUser >= 0 {
		if changed {
			_ = writeAdvisorState(path, state)
		}
		retryReason = persistCodexRetry(path, session, codexRetryReason(records, latestUser, repository))
		state = readAdvisorState(path, session)
	}
	var reasons []string
	kinds := map[string]bool{}
	add := func(kind, reason string) { reasons = append(reasons, reason); kinds[kind] = true }
	planReason := ""
	if latestUser >= 0 {
		if plans, err := ReconcilePlans(stateRoot(), repository); err == nil {
			type readyPlan struct {
				taskID     string
				count      int
				artifact   string
				milestones []string
			}
			byPlan := map[string]*readyPlan{}
			for _, plan := range plans {
				if plan.UserStopped || plan.Gate != "" {
					continue
				}
				for _, ready := range plan.ReadyMilestones {
					if planMilestoneDispatched(records, latestUser, plan.TaskID, milestoneNumber(ready.ID)) {
						continue
					}
					entry := byPlan[plan.TaskID]
					if entry == nil {
						entry = &readyPlan{taskID: plan.TaskID, count: len(plan.AcceptedMilestones), artifact: plan.ArtifactPath}
						byPlan[plan.TaskID] = entry
					}
					entry.milestones = append(entry.milestones, ready.ID)
				}
			}
			var eligible []string
			for _, plan := range byPlan {
				sort.Strings(plan.milestones)
				eligible = append(eligible, fmt.Sprintf("%s (%d accepted) %s#%s", plan.taskID, plan.count, plan.artifact, strings.Join(plan.milestones, ",")))
			}
			sort.Strings(eligible)
			readySet := strings.Join(eligible, "; ")
			reminded, _ := state["plan_ready_set_reminded"].(map[string]any)
			remindedUser, remindedUserOK := 0, false
			if reminded != nil {
				remindedUser, remindedUserOK = exactInteger(reminded["user_index"])
			}
			if readySet != "" && (!remindedUserOK || remindedUser != latestUser || str(reminded["ready_set"]) != readySet) {
				state["plan_ready_set_reminded"] = map[string]any{"user_index": latestUser, "ready_set": readySet}
				planReason = "This turn, start every listed ready pass through a separate purser:dispatcher agent, in parallel. Use normal routing and review: " + readySet
			}
		}
	}
	if notice, ok := state["notice_pending"].(map[string]any); ok && validNoticePending(notice) && notice["blocked"] != true {
		if !autoCompactModActive() {
			notice["blocked"] = true
			add("compaction", "End your reply with a line `---` followed by this exact four-line blockquote, translated into the user's language while keeping the markdown exactly:\n"+stopBlockquote(notice))
		}
	}
	if pending, ok := state["maintenance_pending"].(map[string]any); ok && pending["blocked"] != true {
		pending["blocked"] = true
		add("maintenance", maintenanceGuidance(pending))
	}
	if pending, ok := state["dispatch_pending"].(map[string]any); ok && pending["blocked"] != true {
		if advice := dispatchAdvice(pending); advice != "" {
			pending["blocked"] = true
			add("dispatch", advice)
		}
	}
	if pending, ok := state["version_pending"].(map[string]any); ok && str(pending["version"]) != "" && pending["blocked"] != true {
		pending["blocked"] = true
		add("version", "Include one visible line containing both `⚓` and `"+str(pending["version"])+"` to announce the Purser update.")
	}
	if pending, ok := state["leader_pending"].(map[string]any); ok && str(pending["model"]) != "" && pending["blocked"] != true {
		pending["blocked"] = true
		add("leader", "Include one visible line containing both `⚓` and `"+str(pending["model"])+"` to announce the new free mechanical model leader.")
	}
	if pending, ok := state["fallback_pending"].(map[string]any); ok && (pending["notice"] == true || pending["blocked"] != true) {
		pending["blocked"] = true
		add("fallback", fallbackGuidance(pending))
	}
	blockedUser := -1
	if value, ok := exactInteger(state["flow_blocked_user"]); ok {
		blockedUser = value
	}
	flow, blockedUser := stopFlowReason(records, latestUser, blockedUser, len(reasons) > 0 || retryReason != "")
	if flow != "" {
		state["flow_blocked_user"] = blockedUser
		kinds["flow"] = true
	}
	reasons = stopReasons(retryReason, reasons, flow)
	if retryReason != "" {
		kinds["dispatch"] = true
	}
	if planReason != "" {
		reasons = append(reasons, planReason)
		kinds["plan"] = true
	}
	if changed || len(reasons) > 0 {
		if writeAdvisorState(path, state) != nil {
			return nil
		}
	}
	if pending, ok := state["version_pending"].(map[string]any); ok && pending["blocked"] == true && len(reasons) > 0 {
		_ = writeJSONAtomically(filepath.Join(stateRoot(), "plugin-version.json"), map[string]any{"announced_version": pending["version"]})
	}
	if len(reasons) == 0 {
		return nil
	}
	keys := make([]string, 0, len(kinds))
	for kind := range kinds {
		keys = append(keys, kind)
	}
	sort.Strings(keys)
	emitStopBlocked(session, keys, len(reasons))
	return []byte(`{"decision": "block", "reason": ` + jsonString(strings.Join(reasons, "\n\n")+stopSuffix) + `}`)
}

func stopBlockquote(n map[string]any) string {
	context := number(n["context"])
	percentage := number(n["percentage"])
	return fmt.Sprintf("> ⚓ **Context passed %dK. Purser prepared your handoff.**\n> *Type `/compact` now to save **~%d%%** on every later message!*\n> *The ship stays dry: decisions and the next step will be read from the handoff Purser prepared. Nothing is lost.*\n> *Handoff: `%s`*", context/1000, percentage, str(n["handoff"]))
}

func autoCompactModActive() bool {
	if value := strings.ToLower(os.Getenv("PURSER_AUTO_COMPACT_TOKENS")); value == "0" || value == "off" {
		return false
	}
	info, err := os.Stat(filepath.Join(StateRoot(), "auto-compact.log"))
	if err != nil {
		return false
	}
	age := time.Since(info.ModTime())
	return age >= 0 && age <= 30*time.Minute
}

func fallbackGuidance(p map[string]any) string {
	if p["notice"] == true {
		return "On your next reply, render exactly one line in the user's language as `> ⚓ **...**`. Translate the line below while keeping ⚓ and the model name; do not add another notice line.\n\n" + str(p["line"])
	}
	return "On your next reply, render exactly one bold quoted question line in the user's language as `> ⚓ **...**`. Translate the line below while keeping ⚓ and the model name; include no other question line.\n\n" + str(p["line"])
}

func dispatchAdvice(p map[string]any) string {
	lines, ok := p["lines"].([]any)
	if !ok {
		return ""
	}
	board, hasBoard := p["board"].(string)
	if len(lines) == 0 {
		if hasBoard {
			return "Include this latest job board verbatim:\n" + board
		}
		return ""
	}
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, str(line))
	}
	advice := "Write one line in the user's language as `> ⚓ **Codex: ...**`, including every exact dispatch receipt line below verbatim:\n" + strings.Join(parts, "\n")
	if hasBoard {
		advice += "\nInclude this latest job board verbatim:\n" + board
	}
	return advice
}

func validStopPending(field string, p map[string]any) bool {
	switch field {
	case "notice_pending":
		return validNoticePending(p)
	case "receipt_pending":
		_, effort := p["effort"].(string)
		return str(p["model"]) != "" && effort
	case "dispatch_pending":
		lines, ok := p["lines"].([]any)
		if !ok {
			return false
		}
		for _, line := range lines {
			if str(line) == "" {
				return false
			}
		}
		if p["board"] != nil {
			if _, ok := p["board"].(string); !ok {
				return false
			}
		}
		if ids, ok := p["board_job_ids"].([]any); ok {
			for _, id := range ids {
				if _, ok := id.(string); !ok {
					return false
				}
			}
		} else if p["board_job_ids"] != nil {
			return false
		}
		return true
	case "version_pending":
		return str(p["version"]) != ""
	case "leader_pending":
		_, successOK := exactInteger(p["successes"])
		_, samplesOK := exactInteger(p["sample_count"])
		return str(p["model"]) != "" && successOK && samplesOK
	case "fallback_pending":
		_, lineOK := p["line"].(string)
		_, projectOK := p["project"].(string)
		_, modelOK := p["model"].(string)
		return lineOK && projectOK && modelOK
	case "maintenance_pending":
		_, fileOK := p["notice_file"].(string)
		_, textOK := p["text"].(string)
		evidence, listOK := p["evidence"].([]any)
		if !fileOK || !textOK || !listOK {
			return false
		}
		for _, item := range evidence {
			if str(item) == "" {
				return false
			}
		}
		return true
	}
	return false
}

func markBoardDisplayed(dispatch map[string]any) {
	ids, _ := dispatch["board_job_ids"].([]any)
	for _, value := range ids {
		id := str(value)
		if len(id) != 64 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
			continue
		}
		path := filepath.Join(stateRoot(), "codex_exec_mcp", "jobs", id+".json")
		job, ok := readSmallObject(path)
		if !ok || job["jobId"] != id || job["state"] == "QUEUED" || job["state"] == "RUNNING" {
			continue
		}
		if root := str(job["repoRoot"]); root != "" {
			digest := sha256.Sum256([]byte(resolve(root)))
			lockPath := filepath.Join(stateRoot(), "codex_exec_mcp", "apply", hex.EncodeToString(digest[:])+".lock")
			if os.MkdirAll(filepath.Dir(lockPath), 0700) != nil {
				continue
			}
			lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				continue
			}
			if info, _ := lock.Stat(); info != nil && info.Size() == 0 {
				_, _ = lock.WriteAt([]byte("0"), 0)
			}
			if tryLock(lock) != nil {
				_ = lock.Close()
				continue
			}
			job, ok = readSmallObject(path)
			if ok && job["jobId"] == id && job["state"] != "QUEUED" && job["state"] != "RUNNING" {
				job["boardDisplayCount"] = number(job["boardDisplayCount"]) + 1
				_ = writeJSONAtomically(path, job)
			}
			_ = unlock(lock)
			_ = lock.Close()
			continue
		}
		job["boardDisplayCount"] = number(job["boardDisplayCount"]) + 1
		_ = writeJSONAtomically(path, job)
	}
}

func markLeaderAnnounced(model string) {
	path := filepath.Join(stateRoot(), "opencode", "leader.json")
	leader, ok := readSmallObject(path)
	if ok && leader["model"] == model {
		leader["announced"] = true
		_ = writeJSONAtomically(path, leader)
	}
}

func markFallbackShown(project, session string) {
	updateFallbackQuestion(project, session, true, false)
}

func maintenanceGuidance(p map[string]any) string {
	cli := `"` + maintenanceCommandPath() + `" maintenance`
	if strings.HasSuffix(str(p["notice_file"]), "consent.json") {
		return "Render the maintenance API-cost consent question below in the user's language, translating from the English text and preserving ⚓ and bold. If the user says yes, run `" + cli + ` consent yes --repo "<repository-root>"` + "`; if no, run `" + cli + ` consent no --repo "<repository-root>"` + "`. Do not infer consent.\n" + str(p["text"])
	}
	noticeFile := str(p["notice_file"])
	instruction := "Render the completed maintenance notice below in the user's language, translating from the English text and keeping ⚓, count, paths, and table. " +
		"If the user wants the module off (or uses the equivalent in their language, including `kapat`), run `" + cli + ` opt-out --repo "<repository-root>"` + "`. " +
		"If the user says not to show it again this cycle, run `" + cli + ` skip-second-show --repo "<repository-root>" --report "` + noticeFile + `"` + "`. "
	if p["report_type"] == nil || p["report_type"] == "cleanup" {
		instruction += "If they answer `clean up` or its equivalent in the user's language (including `temizle`), dispatch one ordinary Codex implementation job routed as subtle. " +
			"Re-verify every listed item against current HEAD and remove exactly the still-unused listed items; " +
			"skip each item that is no longer unused and explain why. After successful cleanup, run `" + cli + ` mark-done --repo "<repository-root>" --report "` + noticeFile + `"` + "` and include one `--acted \"<path_line>\"` argument per item actually removed, copying each `path_line` exactly from the report. Do not invent items or edit unlisted code. " +
			"Any `temizle` cleanup must land as ONE separate commit containing only that cleanup; a single `git revert` must undo it.\n"
	}
	return instruction + str(p["text"])
}

func confirmMaintenanceNotice(path string) {
	if path == "" {
		return
	}
	notice, ok := readSmallObject(path)
	if !ok {
		return
	}
	cycle := str(notice["pending_cycle"])
	if cycle == "" {
		cycle = "no-window"
	}
	show := number(notice["pending_show"])
	if show == 0 {
		show = 1
	}
	skip := notice["pending_skip_second"] == true
	delete(notice, "pending_cycle")
	delete(notice, "pending_show")
	delete(notice, "pending_skip_second")
	notice["pending"] = false
	if cycle == "no-window" {
		notice["shown"] = true
	} else {
		cycles, ok := notice["cycles"].(map[string]any)
		if !ok {
			cycles = map[string]any{}
			notice["cycles"] = cycles
		}
		state, ok := cycles[cycle].(map[string]any)
		if !ok {
			state = map[string]any{}
			cycles[cycle] = state
		}
		state["shows"] = max(number(state["shows"]), show)
		notice["last_shown_cycle"] = cycle
		if skip {
			state["skip_second"] = true
		}
	}
	_ = writeJSONAtomically(path, notice)
}

func emitStopBlocked(session string, kinds []string, count int) {
	root := filepath.Join(stateRoot(), "telemetry", "events")
	stamp := time.Now().UTC()
	id := newEventID()
	if id == "" {
		return
	}
	event := map[string]any{"schema_version": 1, "event_id": id, "ts_utc": stamp.Format(time.RFC3339Nano), "event_type": "STOP_BLOCKED", "task_id": nil, "project_key": nil, "session_id": normalizeTelemetryID(session), "repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil, "payload": map[string]any{"reason_kinds": kinds, "reason_count": count}}
	path := filepath.Join(root, stamp.Format("2006-01"), "session_"+normalizeTelemetryID(session)+".jsonl")
	if os.MkdirAll(root, 0700) != nil {
		return
	}
	lock, err := os.OpenFile(filepath.Join(root, ".append.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer lock.Close()
	if info, _ := lock.Stat(); info != nil && info.Size() == 0 {
		_, _ = lock.WriteAt([]byte("0"), 0)
	}
	if tryLock(lock) != nil {
		return
	}
	defer unlock(lock)
	_ = appendEventOnce(path, event)
}
