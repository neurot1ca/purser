package maintenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/checkreport"
	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/pluginroot"
	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
)

const findingsStart = "```purser-findings-json"

var findingPathLine = regexp.MustCompile(`^(.+?):([0-9]+)$`)

// BuildReportPrompt returns the maintenance worker prompt with its effective
// coverage target and canonical repository path substituted.
func BuildReportPrompt(repository, reportType string, coverageTargetPct float64) (string, error) {
	switch reportType {
	case "cleanup":
		return fmt.Sprintf("Produce a cleanup report for the repository at %s.\n"+
			"This is a strictly read-only inspection. Do not edit files, create repository files,\n"+
			"install tools, or run package managers. Detect languages from `git ls-files -co --exclude-standard`\n"+
			"extensions, go.mod, package.json, Cargo.toml, pyproject.toml/setup.cfg, and shebangs.\n"+
			"Use only locally resolved, already-installed tools; never\n"+
			"download tools, dependencies, or toolchains (including npx fetches, `go run\n"+
			"pkg@version`, or `cargo install`). Report only evidence-backed candidates.\n"+
			"For Go use deadcode, else staticcheck -checks U1000; TS/JS use project-local knip\n"+
			"or ts-prune; Rust use compiler dead_code warnings (`cargo check --offline` only if\n"+
			"it needs no download); Python use vulture. Other languages use an existing configured\n"+
			"analyzer. If none is usable, use search evidence and state its limits for that language.\n"+
			"Include a compact per-language scope table: language, file count, tool+version or\n"+
			"search-only/not-scanned, command, and limitation. For each item include exact tool\n"+
			"output or search evidence, repository path and symbol, and checks against entry points,\n"+
			"hooks, and dynamic/reflection use before calling it unused. Include commands/searches\n"+
			"and distinguish uncertain candidates from confirmed facts.\n"+
			"For each finding include why removing it benefits the repository or reduces a risk,\n"+
			"in one short sentence of at most 80 characters.\n"+
			"Order findings from most to least important. Every finding must include severity (high,\n"+
			"medium, or low), meaning its impact if left, separately from confidence.\n"+
			"If user-initiated work starts, it has priority: do not delay it, cancel it, or\n"+
			"continue consuming resources once it needs the machine. End your report with this\n"+
			"machine-readable list, using one object per finding and an empty list for no findings:\n"+
			"```purser-findings-json\n"+
			"[{\"path_line\":\"relative/path.py:12\",\"symbol\":\"name\",\"why\":\"short reason to remove it\",\"evidence\":\"short evidence\",\"severity\":\"high|medium|low\",\"confidence\":\"high|medium|low\"}]\n"+
			"```\n", repository), nil
	case "test-gaps":
		return fmt.Sprintf("Produce a test-gaps report for the repository at %s.\n"+
			"This is a strictly read-only inspection. Do not edit files, create repository files,\n"+
			"install tools, or run package managers. Detect languages from `git ls-files -co --exclude-standard`\n"+
			"extensions, go.mod, package.json, Cargo.toml, pyproject.toml/setup.cfg, and shebangs.\n"+
			"Use only locally resolved, already-installed tools; never\n"+
			"download tools, dependencies, toolchains, or coverage providers (including npx\n"+
			"fetches, `go run pkg@version`, or `cargo install`). Report only evidence-bound tests.\n"+
			"For each candidate, give the target path:line and symbol, the untested behaviour,\n"+
			"evidence from source, the exact command used to search existing tests and its result\n"+
			"showing no test already covers that behaviour, and a high, medium, or low confidence\n"+
			"level. Include a compact contract evidence section covering documented requirements,\n"+
			"invariants, boundaries, and affected callers, cite each item to a source path:line,\n"+
			"and write \"unknown\" where evidence is absent.\n"+
			"Order findings from most to least important. Every finding must include severity (high,\n"+
			"medium, or low), meaning its impact if left, separately from confidence.\n"+
			"For each finding include why the missing test matters in one short sentence\n"+
			"of at most 80 characters.\n"+
			"Measure each language with existing local tools/data: Go existing coverprofile or\n"+
			"`go test -cover` (statement coverage); TS/JS configured jest, vitest, or c8 coverage;\n"+
			"Rust installed cargo llvm-cov or tarpaulin; Python existing data via `python -m\n"+
			"coverage report`; others an existing configured command/artifact. Never fetch a tool,\n"+
			"provider, or dependency. If unavailable, use search evidence and state its limits.\n"+
			"Include a compact per-language scope table: language, file count, tool+version or search-only/\n"+
			"not-scanned, command, limitation. State coverage per language and overall against a\n"+
			"%g%% line-coverage target; order candidate tests by how much uncovered code they\n"+
			"exercise. line_coverage_pct is the overall number or null if any detected language\n"+
			"lacks a measurement.\n"+
			"If user-initiated work starts, it has priority: do not delay it, cancel it, or\n"+
			"continue consuming resources once it needs the machine. End with one JSON object in\n"+
			"this machine-readable block. Findings use the cleanup shape; line_coverage_pct must\n"+
			"be the overall percentage or null:\n"+
			"```purser-findings-json\n"+
			"{\"findings\":[{\"path_line\":\"relative/path.py:12\",\"symbol\":\"name\",\"why\":\"short reason this test matters\",\"evidence\":\"source evidence; untested behaviour; test search command and no-match result\",\"severity\":\"high|medium|low\",\"confidence\":\"high|medium|low\"}],\"line_coverage_pct\":null}\n"+
			"```\n", repository, coverageTargetPct), nil
	default:
		return "", fmt.Errorf("unsupported maintenance report type: %s", reportType)
	}
}

// ParseFindings parses the worker's final structured block. Any malformed
// evidence returns nil (unknown), preserving Python's fail-closed behavior.
func ParseFindings(body string, reportTypes ...string) []map[string]any {
	value, ok := findingsJSON(body)
	if !ok {
		return nil
	}
	if object, isObject := value.(map[string]any); isObject {
		coverage, exists := object["line_coverage_pct"]
		if !exists || len(object) != 2 || (coverage != nil && !validCoverage(coverage)) {
			return nil
		}
		value = object["findings"]
	}
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(list))
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok || len(item) < 4 || len(item) > 6 {
			return nil
		}
		pathLine, pathOK := item["path_line"].(string)
		symbol, symbolOK := item["symbol"].(string)
		evidence, evidenceOK := item["evidence"].(string)
		confidence, confidenceOK := item["confidence"].(string)
		severity, _ := item["severity"].(string)
		_, hasSeverity := item["severity"]
		why, hasWhy := item["why"]
		if len(item) == 5 && !hasWhy && !hasSeverity {
			return nil
		}
		if hasWhy {
			if _, ok := why.(string); !ok {
				return nil
			}
		}
		if !pathOK || findingPathLine.FindStringSubmatch(pathLine) == nil || !symbolOK || strings.TrimSpace(symbol) == "" || !evidenceOK || strings.TrimSpace(evidence) == "" || !confidenceOK || strings.TrimSpace(confidence) == "" {
			return nil
		}
		rank := confidenceRank(confidence)
		if rank == 0 {
			return nil
		}
		severityRank := confidenceRank(severity)
		if severityRank == 0 {
			severity = confidence
			severityRank = rank
		}
		finding := map[string]any{"path_line": pathLine, "symbol": symbol, "evidence": evidence, "confidence": confidence, "confidence_rank": rank, "severity": strings.ToLower(severity), "severity_rank": severityRank}
		if hasWhy {
			finding["why"] = why
		}
		result = append(result, finding)
	}
	// Stable sorting preserves worker order when both importance and confidence tie.
	sort.SliceStable(result, func(i, j int) bool {
		if result[i]["severity_rank"].(int) != result[j]["severity_rank"].(int) {
			return result[i]["severity_rank"].(int) > result[j]["severity_rank"].(int)
		}
		return result[i]["confidence_rank"].(int) > result[j]["confidence_rank"].(int)
	})
	return result
}

func LineCoverage(body string) any {
	value, ok := findingsJSON(body)
	if !ok {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	coverage := object["line_coverage_pct"]
	if validCoverage(coverage) {
		return coverage
	}
	return nil
}

func findingsJSON(body string) (any, bool) {
	start := strings.LastIndex(body, findingsStart)
	if start < 0 {
		return nil, false
	}
	start += len(findingsStart)
	end := strings.Index(body[start:], "```")
	if end < 0 {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(body[start : start+end])))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, false
	}
	return value, true
}

func validCoverage(value any) bool {
	n, ok := number(value)
	return ok && n >= 0 && n <= 100
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func confidenceRank(value string) int {
	switch strings.ToLower(value) {
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

// PersistReport writes the report, last-run marker, and Python-compatible
// notice record. Timestamp must use Python's YYYYmmddTHHMMSS.ffffffZ format.
func PersistReport(state, repository, reportType, timestamp, commit, body string, versions []string, limitations string, findings []map[string]any, coverage any, coverageTargetPct float64, cost map[string]any) (string, error) {
	project := filepath.Join(state, "projects", contextio.ProjectKey(repository), "maintenance")
	reports := filepath.Join(project, "reports", reportType)
	path := filepath.Join(reports, timestamp+".md")
	title := "Cleanup"
	if reportType == "test-gaps" {
		title = "Test gaps"
	}
	findingCount := any("unknown")
	if findings != nil {
		findingCount = len(findings)
	}
	if cost == nil {
		cost = map[string]any{"tokens": "unknown", "elapsed_seconds": "unknown", "quota_change": "unknown"}
	}
	encodedCost, err := json.Marshal(cost)
	if err != nil {
		return "", err
	}
	// Python json.dumps(sort_keys=True) is reproduced for stable report text.
	var sorted map[string]any
	_ = json.Unmarshal(encodedCost, &sorted)
	keys := make([]string, 0, len(sorted))
	for key := range sorted {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var costText strings.Builder
	costText.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			costText.WriteString(", ")
		}
		keyBytes, _ := json.Marshal(key)
		valueBytes, _ := json.Marshal(sorted[key])
		_, _ = fmt.Fprintf(&costText, "%s: %s", keyBytes, valueBytes)
	}
	costText.WriteByte('}')
	content := fmt.Sprintf("# %s report\n\n- Source commit: `%s`\n- Run time (UTC): `%s`\n- Tool versions: %s\n- Limitations: %s\n\n- Findings: %v\n- Cost: `%s`\n\n%s\n", title, commit, timestamp, strings.Join(versions, ", "), limitations, findingCount, costText.String(), strings.TrimRight(body, " \t\r\n"))
	if err = writeAtomic(path, []byte(content)); err != nil {
		return "", err
	}
	if err = writeJSONAtomic(lastRunPath(project, reportType), map[string]any{"completed_at": float64(time.Now().UnixNano()) / 1e9}); err != nil {
		return "", err
	}
	notice := map[string]any{"report_type": reportType, "report_path": path, "findings": findings, "finding_count": nullableCount(findings), "line_coverage_pct": coverage, "coverage_target_pct": coverageTargetPct, "shown": false}
	if err = writeJSONAtomic(strings.TrimSuffix(path, ".md")+".notice.json", notice); err != nil {
		return "", err
	}
	return path, nil
}

func lastRunPath(project, reportType string) string {
	if reportType == "cleanup" {
		return filepath.Join(project, "last-run.json")
	}
	return filepath.Join(project, "reports", reportType, "last-run.json")
}

func nullableCount(findings []map[string]any) any {
	if findings == nil {
		return nil
	}
	return len(findings)
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".maintenance-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// RunAndPersistReport is the pass-5 prompt -> Runner -> parse -> persistence
// composition. Eligibility and opt-in stay with pass 6.
func RunAndPersistReport(r Runner, state, repository, reportType string) (string, error) {
	if err := r.resolveCoder(); err != nil {
		return "", err
	}
	root, err := filepath.Abs(repository)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	coverageTargetPct := profile.LoadProfile(root).CoverageTargetPct
	prompt, err := BuildReportPrompt(root, reportType, coverageTargetPct)
	if err != nil {
		return "", err
	}
	started := time.Now()
	timestamp := started.UTC().Format("20060102T150405.000000Z")
	quotaBefore := latestMaintenanceQuota(time.Now(), r.CoderAgent)
	jobsBefore := maintenanceJobIDs(state)
	commit := "unknown"
	command := proc.Hide(exec.Command("git", "-C", root, "rev-parse", "HEAD"))
	if output, gitErr := command.Output(); gitErr == nil {
		commit = strings.TrimSpace(string(output))
	}
	versions := []string{"Runtime: " + checkreport.RuntimeDetail(maintenancePluginRoot()), gitVersion(root)}
	limitations := "Coverage availability and result are reported by the worker."
	if reportType == "cleanup" {
		versions = append(versions, vultureVersion())
		limitations = "Vulture available; worker must still check entry points, hooks, and dynamic use."
		if strings.HasPrefix(versions[2], "vulture: unavailable") {
			limitations = "Vulture unavailable; grep-based search only, which can miss indirect and dynamic use."
		}
	}
	result, runErr := r.Run(state, root, reportType, prompt)
	if runErr != nil {
		recordReportFailure(state, root, reportType, timestamp, result, runErr, started, quotaBefore, r.CoderAgent)
		return "", runErr
	}
	body := stringValue(result["content"], "")
	findings := ParseFindings(body, reportType)
	coverage := any(nil)
	if reportType == "test-gaps" {
		coverage = LineCoverage(body)
	}
	tokens := any("unknown")
	if usage, ok := result["usage"].(map[string]any); ok && len(usage) > 0 {
		tokens = usage
	} else if value := result["tokens"]; value != nil && value != 0 && value != "" {
		tokens = value
	} else if value := result["totalTokens"]; value != nil && value != 0 && value != "" {
		tokens = value
	}
	quotaAfter := latestMaintenanceQuota(time.Now(), r.CoderAgent)
	jobID, _ := result["jobId"].(string)
	quotaChange := maintenanceQuotaChange(quotaBefore, quotaAfter, jobsBefore, maintenanceJobIDs(state), jobID, state)
	cost := map[string]any{"tokens": tokens, "elapsed_seconds": roundSeconds(time.Since(started)), "quota_before": quotaOrUnknown(quotaBefore), "quota_after": quotaOrUnknown(quotaAfter), "quota_change_active_window_pct": quotaChange}
	path, err := PersistReport(state, root, reportType, timestamp, commit, body, versions, limitations, findings, coverage, coverageTargetPct, cost)
	if err != nil {
		recordReportFailure(state, root, reportType, timestamp, result, err, started, quotaBefore, r.CoderAgent)
		return "", err
	}
	counts := map[string]int{}
	for _, item := range findings {
		counts[strings.ToLower(item["confidence"].(string))]++
	}
	payload := map[string]any{"report_id": timestamp, "report_type": reportType, "outcome": "completed", "reason_code": "COMPLETED", "elapsed_seconds": roundSeconds(time.Since(started)), "findings_high_count": nullableConfidence(findings, counts, "high"), "findings_medium_count": nullableConfidence(findings, counts, "medium"), "findings_low_count": nullableConfidence(findings, counts, "low")}
	if value, ok := quotaChange.(float64); ok {
		payload["quota_change_active_window_pct"] = value
	} else {
		payload["quota_change_active_window_pct"] = nil
	}
	if reportType == "test-gaps" {
		payload["line_coverage_pct"] = coverage
	} else {
		payload["line_coverage_pct"] = nil
	}
	enforce.EmitMaintenanceReportObservation(state, root, "MAINTENANCE_REPORT_FINISHED", payload)
	return path, nil
}

func nullableConfidence(findings []map[string]any, counts map[string]int, key string) any {
	if findings == nil {
		return nil
	}
	return counts[key]
}

func latestMaintenanceQuota(now time.Time, agent string) map[string]any {
	if agent != "codex" {
		return nil
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(userHome, ".codex")
	}
	value, ok := contextio.LatestQuota(home, now)
	if !ok || value == nil {
		return nil
	}
	stamp, ok := value["snapshot_at"].(string)
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if !ok || err != nil || at.After(now) || now.Sub(at) > 30*time.Minute {
		return nil
	}
	filtered := map[string]any{"snapshot_at": stamp}
	if windows, ok := value["windows_seen"].(bool); ok {
		filtered["windows_seen"] = windows
	}
	for _, key := range []string{"5h_used_pct", "5h_resets_at", "7d_used_pct", "7d_resets_at"} {
		if item, exists := value[key]; exists {
			filtered[key] = item
		}
	}
	return filtered
}

func quotaOrUnknown(quota map[string]any) any {
	if len(quota) == 0 {
		return "unknown"
	}
	return quota
}

func maintenanceQuotaChange(before, after map[string]any, jobsBefore, jobsAfter map[string]bool, jobID, state string) any {
	beforeWindow, beforeOK := maintenanceQuotaWindow(before)
	afterWindow, afterOK := maintenanceQuotaWindow(after)
	if !beforeOK || !afterOK || beforeWindow[0] != afterWindow[0] || beforeWindow[2] != afterWindow[2] || jobsBefore == nil || jobsAfter == nil {
		return "unknown"
	}
	for id := range jobsAfter {
		if _, existed := jobsBefore[id]; !existed && id != jobID {
			return "unknown"
		}
		if id == jobID {
			continue
		}
		var record map[string]any
		data, err := os.ReadFile(filepath.Join(state, "codex_exec_mcp", "jobs", id+".json"))
		if err != nil || json.Unmarshal(data, &record) != nil || record["state"] == "RUNNING" || record["state"] == "QUEUED" {
			return "unknown"
		}
	}
	beforeUsed, _ := number(beforeWindow[1])
	afterUsed, _ := number(afterWindow[1])
	return roundFloat(afterUsed - beforeUsed)
}

func maintenanceQuotaWindow(value map[string]any) ([3]any, bool) {
	for _, label := range []string{"5h", "7d"} {
		used, usedOK := number(value[label+"_used_pct"])
		reset, resetOK := value[label+"_resets_at"].(string)
		if usedOK && resetOK && reset != "" {
			return [3]any{label, used, reset}, true
		}
	}
	return [3]any{}, false
}

func maintenanceJobIDs(state string) map[string]bool {
	dir := filepath.Join(state, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}
	}
	if err != nil {
		return nil
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			ids[strings.TrimSuffix(entry.Name(), ".json")] = true
		}
	}
	return ids
}

func roundFloat(value float64) float64 {
	return math.RoundToEven(value*1000) / 1000
}

func recordReportFailure(state, root, reportType, timestamp string, result map[string]any, cause error, started time.Time, quotaBefore map[string]any, agent string) {
	stateName := stringValue(result["state"], "FAILED")
	errorText := cause.Error()
	failure := map[string]any{"project_root": root, "started_at": timestamp, "job_id": result["jobId"], "state": "FAILED", "error": errorText, "cost": map[string]any{"tokens": "unknown", "elapsed_seconds": roundSeconds(time.Since(started)), "quota_before": quotaOrUnknown(quotaBefore), "quota_after": quotaOrUnknown(latestMaintenanceQuota(time.Now(), agent)), "quota_change_active_window_pct": "unknown"}}
	failurePath := filepath.Join(state, "projects", contextio.ProjectKey(root), "maintenance", "reports", reportType, "failures", timestamp+".json")
	_ = writeJSONAtomic(failurePath, failure)
	outcome, reason := "failed", "REPORT_FAILED"
	if strings.Contains(errorText, "yielded to a user "+coderLabel(agent)+" job") {
		outcome, reason = "yielded", "USER_JOB_PREEMPTED"
	} else if strings.Contains(errorText, coderLabel(agent)+" job ") && agent == "codex" {
		reason = "CODEX_JOB_" + stateName
	}
	_ = enforce.EmitMaintenanceReportObservation(state, root, "MAINTENANCE_REPORT_FINISHED", map[string]any{"report_id": timestamp, "report_type": reportType, "outcome": outcome, "reason_code": reason, "elapsed_seconds": roundSeconds(time.Since(started)), "quota_change_active_window_pct": nil, "findings_high_count": nil, "findings_medium_count": nil, "findings_low_count": nil, "line_coverage_pct": nil})
}

func roundSeconds(d time.Duration) float64 { return float64(int64(d.Seconds()*1000+0.5)) / 1000 }

func gitVersion(root string) string {
	command := proc.Hide(exec.Command("git", "-C", root, "--version"))
	if out, err := command.Output(); err == nil {
		return "Git: " + strings.TrimSpace(string(out))
	}
	return "Git: unavailable"
}

func maintenancePluginRoot() string {
	return pluginroot.Root()
}

func vultureVersion() string {
	path, err := exec.LookPath("vulture")
	if err != nil {
		return "vulture: unavailable"
	}
	command := proc.Hide(exec.Command(path, "--version"))
	out, err := command.CombinedOutput()
	if err != nil {
		return "vulture: version unavailable"
	}
	return "vulture: " + strings.TrimSpace(string(bytes.TrimSpace(out)))
}
