package contextio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

func queueMaintenanceNotice(statePath, repository, key, session string) {
	defer func() { _ = recover() }()
	if statePath == "" || repository == "" || session == "" {
		return
	}
	directory := filepath.Join(stateRoot(), "projects", key, "maintenance")
	if !maintenanceOptedIn(directory, repository) {
		return
	}
	quota, window, ok := latestMaintenanceQuota()
	if !ok || window == nil {
		return // UserPromptSubmit uses allow_no_window=false.
	}
	var candidates []string
	for _, reportType := range []string{"cleanup", "test-gaps"} {
		paths, _ := filepath.Glob(filepath.Join(directory, "reports", reportType, "*.notice.json"))
		candidates = append(candidates, paths...)
	}
	sort.Strings(candidates)
	for _, path := range candidates {
		notice, valid := readSmallObject(path)
		if !valid || notice["done"] == true || notice["pending"] == true {
			continue
		}
		if notice["report_type"] == nil {
			if filepath.Base(filepath.Dir(path)) == "test-gaps" {
				notice["report_type"] = "test-gaps"
			} else {
				notice["report_type"] = "cleanup"
			}
		}
		if findings, ok := notice["findings"].([]any); ok {
			staleCount, verified := dropStaleMaintenanceFindings(repository, notice)
			if verified {
				if staleCount == 0 {
					notice["done"], notice["resolved"], notice["pending"] = true, true, false
					_ = writeJSONAtomically(path, notice)
					continue
				} else if updated, ok := notice["findings"].([]any); ok && len(updated) != len(findings) {
					_ = writeJSONAtomically(path, notice)
				}
				if reverifyMaintenanceFindings(repository, notice) == 0 {
					notice["done"], notice["resolved"], notice["pending"] = true, true, false
					_ = writeJSONAtomically(path, notice)
					continue
				} else if updated, ok := notice["findings"].([]any); ok && len(updated) != len(findings) {
					_ = writeJSONAtomically(path, notice)
				}
			}
		}
		show, cycle, skipSecond := maintenanceShow(notice, quota, window)
		if !show {
			continue
		}
		notice["pending"], notice["pending_cycle"], notice["pending_show"] = true, cycle, showNumber(notice, cycle, window)
		if window == nil {
			notice["pending_show"] = 1
		}
		if skipSecond {
			notice["pending_skip_second"] = true
		}
		if window != nil {
			notice["quota_window"] = window[0]
		}
		if writeJSONAtomically(path, notice) != nil {
			continue
		}
		evidence := []string{}
		if reportPath, ok := notice["report_path"].(string); ok && filepath.Base(reportPath) != "." {
			evidence = append(evidence, filepath.Base(reportPath))
		}
		if findings, ok := notice["findings"].([]any); ok && len(findings) > 0 {
			if first, ok := findings[0].(map[string]any); ok {
				if line := str(first["path_line"]); line != "" {
					evidence = append(evidence, line)
				}
			}
		}
		state := readAdvisorState(statePath, session)
		reportType, _ := notice["report_type"].(string)
		if reportType == "" {
			reportType = "cleanup"
		}
		old, _ := state["maintenance_pending"].(map[string]any)
		text := formatMaintenanceNotice(notice)
		delivered := old != nil && old["notice_file"] == path && old["text"] == text && old["delivered"] == true
		state["maintenance_pending"] = map[string]any{"notice_file": path, "text": text, "evidence": evidence, "report_type": reportType, "blocked": false, "delivered": delivered}
		_ = writeAdvisorState(statePath, state)
		return
	}
}

func dropStaleMaintenanceFindings(repository string, notice map[string]any) (int, bool) {
	rawFindings, ok := notice["findings"].([]any)
	if !ok {
		return 0, false
	}
	reportPath := str(notice["report_path"])
	if reportPath == "" {
		return len(rawFindings), false
	}
	report, err := os.ReadFile(reportPath)
	if err != nil || len(report) > 1<<20 {
		return len(rawFindings), false
	}
	const sourcePrefix = "- Source commit: `"
	var source string
	for _, line := range strings.Split(string(report), "\n") {
		if strings.HasPrefix(line, sourcePrefix) && strings.HasSuffix(line, "`") {
			source = strings.TrimSuffix(strings.TrimPrefix(line, sourcePrefix), "`")
			break
		}
	}
	if len(source) < 40 || len(source) > 64 {
		return len(rawFindings), false
	}
	for _, r := range source {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return len(rawFindings), false
		}
	}
	changed, err := gitOutput(repository, "diff", "--name-only", source+"..HEAD")
	if err != nil {
		return len(rawFindings), false
	}
	changedPaths := map[string]struct{}{}
	for _, path := range strings.Split(strings.ReplaceAll(string(changed), "\\", "/"), "\n") {
		if path != "" {
			changedPaths[path] = struct{}{}
		}
	}
	remaining := make([]any, 0, len(rawFindings))
	for _, raw := range rawFindings {
		item, ok := raw.(map[string]any)
		if !ok {
			remaining = append(remaining, raw)
			continue
		}
		pathLine := str(item["path_line"])
		rel := strings.TrimSpace(strings.SplitN(pathLine, ":", 2)[0])
		if rel != "" && maintenanceFindingOrTestChanged(filepath.ToSlash(filepath.Clean(rel)), changedPaths) {
			continue
		}
		remaining = append(remaining, raw)
	}
	notice["findings"] = remaining
	notice["finding_count"] = len(remaining)
	if len(remaining) == 0 {
		notice["done"], notice["resolved"], notice["pending"] = true, true, false
	}
	return len(remaining), true
}

func maintenanceFindingOrTestChanged(path string, changed map[string]struct{}) bool {
	if _, ok := changed[path]; ok {
		return true
	}
	dir, name := filepath.ToSlash(filepath.Dir(path)), filepath.Base(path)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	testNames := []string{base + ".test" + ext, base + "_test" + ext}
	if ext == ".go" {
		for _, platform := range []string{"_windows", "_linux", "_darwin", "_freebsd", "_openbsd", "_netbsd", "_amd64", "_arm64"} {
			testNames = append(testNames, base+platform+"_test"+ext)
		}
	}
	for _, testName := range testNames {
		testPath := testName
		if dir != "." {
			testPath = dir + "/" + testName
		}
		if _, ok := changed[testPath]; ok {
			return true
		}
	}
	return false
}

func latestMaintenanceQuota() (map[string]any, *[3]any, bool) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, false
		}
		home = filepath.Join(userHome, ".codex")
	}
	return latestQuotaAt(home, time.Now().UTC())
}

func latestQuotaAt(home string, now time.Time) (map[string]any, *[3]any, bool) {
	now = now.UTC()
	paths := []string{}
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		matches, _ := filepath.Glob(filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"), "rollout-*.jsonl"))
		paths = append(paths, matches...)
	}
	var latest string
	var modified time.Time
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && (latest == "" || info.ModTime().After(modified)) {
			latest, modified = path, info.ModTime()
		}
	}
	if latest == "" {
		return nil, nil, false
	}
	data, err := os.ReadFile(latest)
	if err != nil {
		return nil, nil, false
	}
	if len(data) > 256<<10 {
		data = data[len(data)-(256<<10):]
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := strings.Split(string(data), "\n")
	var event map[string]any
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := map[string]any{}
		if json.Unmarshal([]byte(lines[i]), &candidate) != nil {
			continue
		}
		payload, _ := candidate["payload"].(map[string]any)
		if _, ok := payload["rate_limits"].(map[string]any); ok {
			event = candidate
			break
		}
	}
	if event == nil {
		return nil, nil, false
	}
	stamp, hasStamp := event["timestamp"].(string)
	payload, _ := event["payload"].(map[string]any)
	limits, _ := payload["rate_limits"].(map[string]any)
	sourcePath, _ := filepath.Abs(home)
	if real, e := filepath.EvalSymlinks(sourcePath); e == nil {
		sourcePath = real
	}
	sourceHash := sha256.Sum256([]byte(filepath.Clean(sourcePath)))
	snapshot := map[string]any{"windows_seen": false, "source_identity": hex.EncodeToString(sourceHash[:])}
	if hasStamp {
		snapshot["snapshot_at"] = stamp
	}
	planType, _ := payload["plan_type"].(string)
	if planType == "" {
		planType, _ = limits["plan_type"].(string)
	}
	if planType != "" {
		snapshot["plan_type"] = strings.ToLower(strings.TrimSpace(planType))
	}
	for _, entry := range []struct {
		source, label string
		minutes       int
	}{{"primary", "5h", 300}, {"secondary", "7d", 10080}} {
		raw, _ := limits[entry.source].(map[string]any)
		minutes, mok := exactInteger(raw["window_minutes"])
		used, uok := raw["used_percent"].(float64)
		reset, rok := raw["resets_at"].(float64)
		if mok && minutes == entry.minutes && uok && !math.IsNaN(used) && !math.IsInf(used, 0) && rok && !math.IsNaN(reset) && !math.IsInf(reset, 0) {
			snapshot["windows_seen"] = true
			resetAt := time.UnixMicro(int64(math.Round(reset * 1e6))).UTC()
			if resetAt.After(now) {
				snapshot[entry.label+"_used_pct"] = used
				layout := "2006-01-02T15:04:05Z"
				if resetAt.Nanosecond() != 0 {
					layout = "2006-01-02T15:04:05.000000Z"
				}
				snapshot[entry.label+"_resets_at"] = resetAt.Format(layout)
			}
		}
	}
	for _, label := range []string{"5h", "7d"} {
		if used, ok := snapshot[label+"_used_pct"].(float64); ok {
			reset := str(snapshot[label+"_resets_at"])
			parsed, e := time.Parse(time.RFC3339Nano, reset)
			if e == nil && parsed.After(now) {
				return snapshot, &[3]any{label, used, reset}, true
			}
		}
	}
	return snapshot, nil, true
}

// LatestQuota exposes the quota snapshot used by synchronous Pre decisions.
func LatestQuota(home string, now time.Time) (map[string]any, bool) {
	quota, _, ok := latestQuotaAt(home, now)
	return quota, ok
}

func maintenanceShow(notice, quota map[string]any, window *[3]any) (bool, string, bool) {
	if window == nil {
		return false, "", false
	}
	label := window[0].(string)
	used := window[1].(float64)
	reset := window[2].(string)
	remaining := 100 - used
	cycles, _ := notice["cycles"].(map[string]any)
	if cycles == nil {
		cycles = map[string]any{}
		notice["cycles"] = cycles
	}
	cycle, _ := cycles[reset].(map[string]any)
	if cycle == nil {
		cycle = map[string]any{}
	}
	shows := number(cycle["shows"])
	totalShows := 0
	for _, raw := range cycles {
		if entry, ok := raw.(map[string]any); ok {
			totalShows += number(entry["shows"])
		}
	}
	if cycle["skip_second"] == true || totalShows >= 2 {
		return false, "", false
	}
	if shows == 0 && remaining > 70 || shows == 1 && remaining > 30 {
		return false, "", false
	}
	show := shows + 1
	notice["pending_skip_second"] = show == 1 && remaining <= 30
	notice["quota_window"] = label
	_ = quota
	return true, reset, show == 1 && remaining <= 30
}

func showNumber(notice map[string]any, cycle string, window *[3]any) int {
	if window == nil {
		return 1
	}
	cycles, _ := notice["cycles"].(map[string]any)
	entry, _ := cycles[cycle].(map[string]any)
	return number(entry["shows"]) + 1
}

func formatMaintenanceNotice(notice map[string]any) string {
	path := str(notice["report_path"])
	if path == "" {
		path = "unknown"
	}
	count, countOK := exactInteger(notice["finding_count"])
	findings, findingsOK := notice["findings"].([]any)
	if notice["report_type"] == "test-gaps" {
		countText := fmt.Sprint(count)
		if !countOK {
			countText = "an unknown number of"
		}
		lines := []string{}
		target, validTarget := notice["coverage_target_pct"].(float64)
		if !validTarget || math.IsNaN(target) || math.IsInf(target, 0) || target <= 0 || target > 100 {
			target = 90
		}
		if coverage, ok := notice["line_coverage_pct"].(float64); ok && !math.IsNaN(coverage) && !math.IsInf(coverage, 0) && coverage >= target {
			lines = append(lines, fmt.Sprintf("> ⚓ **Purser: your code is now %d%% covered by tests! 🚀** You have reached a top-tier quality bar. Purser found the gaps for you in the background while you focused on your work. Type /purser:optimize for the last few steps.", int(math.Floor(coverage+0.5))))
		}
		lines = append(lines, "> ⚓ **Purser checked your tests in the background while you worked: the report lists "+countText+" missing tests.**")
		if !findingsOK {
			lines = append(lines, "> *The findings could not be parsed; report: `"+path+"`.*")
		} else {
			lines = append(lines, "> *The table below shows each gap, why it matters, and its evidence; the full report is at `"+path+"`.*", "> | Severity | Gap | Why it matters | Evidence |", "> | --- | --- | --- | --- |")
			for _, item := range rankMaintenanceFindings(findings, 5) {
				lines = append(lines, fmt.Sprintf("> | %s | %s | %s | %s |", maintenanceSeverity(item), truncateRunes(maintenanceCandidate(item), 48), maintenanceWhy(item), truncateMaintenanceEvidence(strings.Join(strings.Fields(fmt.Sprint(item["evidence"])), " "), 48)))
			}
		}
		if notice["pending_cycle"] != nil && notice["pending_cycle"] != "no-window" && number(notice["pending_show"]) == 1 && notice["pending_skip_second"] != true {
			lines = append(lines, "> *Skip showing this again in this cycle?*")
		}
		lines = append(lines, "> *Say \"turn off maintenance\" to stop these reports, or run one any time with /purser:optimize.*")
		return strings.Join(lines, "\n")
	}
	intro := "⚓ **While you worked, Purser scanned your project in the background with its proactive cleanup module: the number of findings is unknown.**"
	if countOK {
		intro = fmt.Sprintf("⚓ **While you worked, Purser scanned your project in the background with its proactive cleanup module: the report has %d findings.**", count)
	}
	if !findingsOK {
		return "> " + intro + "\n> *The findings could not be parsed; report: `" + path + "`.*"
	}
	ranked := rankMaintenanceFindings(findings, 5)
	instruction := "The table below lists each finding, why to clean it, and its evidence. Just say \"clean up\" to have them cleaned."
	if len(findings) > 5 {
		instruction = "The table below lists the top 5 findings, why to clean them, and their evidence. Just say \"clean up\" to have them cleaned. The other findings are in this file to save tokens: `" + path + "`"
	}
	lines := []string{"> " + intro, "> *" + instruction + "*", "> | Severity | Finding | Why clean it | Evidence |", "> | --- | --- | --- | --- |"}
	for _, item := range ranked {
		candidate := truncateRunes(maintenanceCandidate(item), 48)
		evidence := truncateMaintenanceEvidence(strings.Join(strings.Fields(fmt.Sprint(item["evidence"])), " "), 48)
		lines = append(lines, fmt.Sprintf("> | %s | %s | %s | %s |", maintenanceSeverity(item), candidate, maintenanceWhy(item), evidence))
	}
	if notice["pending_cycle"] != nil && notice["pending_cycle"] != "no-window" && number(notice["pending_show"]) == 1 && notice["pending_skip_second"] != true {
		lines = append(lines, "> *Skip showing this again in this cycle?*")
	}
	lines = append(lines, "> *Say \"turn off maintenance\" to stop these reports, or run one any time with /purser:optimize.*")
	return strings.Join(lines, "\n")
}

func rankMaintenanceFindings(findings []any, limit int) []map[string]any {
	type rankedFinding struct {
		item           map[string]any
		severityRank   int
		confidenceRank int
	}
	ranked := []rankedFinding{}
	for _, raw := range findings {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		confidenceRank := maintenanceRank(str(item["confidence"]))
		severityRank := maintenanceRank(maintenanceSeverity(item))
		if rank := number(item["confidence_rank"]); rank > 0 {
			confidenceRank = rank
		}
		if rank := number(item["severity_rank"]); rank > 0 {
			severityRank = rank
		}
		ranked = append(ranked, rankedFinding{item, severityRank, confidenceRank})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].severityRank != ranked[j].severityRank {
			return ranked[i].severityRank > ranked[j].severityRank
		}
		return ranked[i].confidenceRank > ranked[j].confidenceRank
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	result := make([]map[string]any, 0, len(ranked))
	for _, entry := range ranked {
		result = append(result, entry.item)
	}
	return result
}

func maintenanceSeverity(item map[string]any) string {
	severity := strings.ToLower(strings.TrimSpace(str(item["severity"])))
	if maintenanceRank(severity) == 0 {
		severity = strings.ToLower(strings.TrimSpace(str(item["confidence"])))
	}
	if maintenanceRank(severity) == 0 {
		return "—"
	}
	return severity
}

func maintenanceRank(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

func maintenanceCandidate(item map[string]any) string {
	path, symbol := str(item["path_line"]), str(item["symbol"])
	if path == "" {
		path = "?"
	}
	if symbol == "" {
		symbol = "?"
	}
	return path + " " + symbol
}

func maintenanceWhy(item map[string]any) string {
	why := strings.Join(strings.Fields(str(item["why"])), " ")
	if why == "" {
		evidence := strings.ToLower(str(item["evidence"]))
		symbol := strings.ToLower(str(item["symbol"]))
		tracked := strings.Contains(evidence, "tracked") && !strings.Contains(evidence, "untracked")
		switch {
		case tracked && (strings.Contains(evidence, "binary") || strings.Contains(evidence, "database") || strings.Contains(symbol, ".coverage")):
			return "tracked build artifact bloats the repo"
		case strings.Contains(evidence, "untracked"):
			return "untracked experiment files add repository clutter"
		default:
			return "reported as unused by the cleanup scan"
		}
	}
	return truncateRunes(why, 64)
}

func truncateMaintenanceEvidence(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return "…"
	}
	end := limit - 1
	for end > 0 && runes[end] != ' ' && runes[end] != '\t' && runes[end] != '\n' {
		end--
	}
	if end == 0 {
		end = limit - 1
	}
	return strings.TrimRight(string(runes[:end]), " \t\n") + "…"
}

func reverifyMaintenanceFindings(repository string, notice map[string]any) int {
	rawFindings, ok := notice["findings"].([]any)
	if !ok {
		return 0
	}
	remaining := make([]any, 0, len(rawFindings))
	for _, raw := range rawFindings {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		pathLine := str(item["path_line"])
		rel := strings.TrimSpace(strings.SplitN(pathLine, ":", 2)[0])
		if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
			continue
		}
		evidence := strings.ToLower(str(item["evidence"]))
		tracked := strings.Contains(evidence, "tracked") && !strings.Contains(evidence, "untracked")
		if _, err := os.Stat(filepath.Join(repository, filepath.FromSlash(rel))); err != nil {
			continue
		}
		if strings.Contains(evidence, "untracked") {
			dir := strings.Split(filepath.ToSlash(rel), "/")[0]
			output, gitErr := gitOutput(repository, "status", "--porcelain", "--untracked-files=all", "--", dir)
			if gitErr != nil {
				remaining = append(remaining, raw)
				continue
			}
			if len(output) == 0 {
				continue
			}
		}
		if tracked {
			output, gitErr := gitOutput(repository, "ls-tree", "-r", "--name-only", "HEAD", "--", filepath.ToSlash(rel))
			if gitErr != nil {
				remaining = append(remaining, raw)
				continue
			}
			if len(output) == 0 {
				continue
			}
		}
		remaining = append(remaining, raw)
	}
	notice["findings"] = remaining
	notice["finding_count"] = len(remaining)
	if len(remaining) == 0 {
		notice["done"], notice["resolved"], notice["pending"] = true, true, false
	}
	return len(remaining)
}

func gitOutput(repository string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := proc.Hide(exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...))
	return command.Output()
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}

func promptMaintenanceGuidance(pending map[string]any) string {
	cli := fmt.Sprintf("\"%s\" maintenance", maintenanceCommandPath())
	text, noticeFile := str(pending["text"]), str(pending["notice_file"])
	if strings.HasSuffix(noticeFile, "consent.json") {
		return fmt.Sprintf("Render the maintenance API-cost consent question below in the user's language, translating from the English text and preserving ⚓ and bold. If the user says yes, run `%s consent yes --repo \"<repository-root>\"`; if no, run `%s consent no --repo \"<repository-root>\"`. Do not infer consent.\n%s", cli, cli, text)
	}
	instruction := "Render the completed maintenance notice below in the user's language, translating from the English text and keeping ⚓, count, paths, and table. If the user wants the module off (or uses the equivalent in their language, including `kapat`), run `" + cli + " opt-out --repo \"<repository-root>\"`. If the user says not to show it again this cycle, run `" + cli + " skip-second-show --repo \"<repository-root>\" --report \"" + noticeFile + "\"`. "
	if pending["report_type"] == nil || pending["report_type"] == "cleanup" {
		instruction += "If they answer `clean up` or its equivalent in the user's language (including `temizle`), dispatch one ordinary Codex implementation job routed as subtle. Re-verify every listed item against current HEAD and remove exactly the still-unused listed items; skip each item that is no longer unused and explain why. After successful cleanup, run `" + cli + " mark-done --repo \"<repository-root>\" --report \"" + noticeFile + "\"` and include one `--acted \"<path_line>\"` argument per item actually removed, copying each `path_line` exactly from the report. Do not invent items or edit unlisted code. Any `temizle` cleanup must land as ONE separate commit containing only that cleanup; a single `git revert` must undo it.\n"
	}
	return instruction + text
}
