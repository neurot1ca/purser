package enforce

import (
	"context"
	"time"

	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

// schedulePostReview performs the PostToolUse check scheduling and Reviewer-1
// gate after delta assembly. It deliberately stops before reviewer selection or
// invocation; post.go remains owned by the Python hook during this tranche.
func schedulePostReview(repoRoot string, pending map[string]any, bundle map[string]any, resolved postProfile, configured []profile.Check, gateConfig map[string]any, checkDeadline time.Duration, reviewerAvailable, routeKnown bool) {
	delta, _ := bundle["delta"].(map[string]any)
	phase := stringValue(pending["phase"])
	if phase == "operation" {
		bundle["reviewer1_result"] = nil
		bundle["reviewer1_skipped_reason"] = review.PhaseOperation
		bundle["claude_review_mode"] = review.Full
		return
	}
	if delta == nil || delta["status"] != "OK" {
		bundle["claude_review_mode"] = review.Full
		return
	}

	paths, pathsOK := postStringList(delta["changed_paths"])
	if !pathsOK {
		paths = nil
	}
	var selected []profile.Check
	if phase != "plan" {
		selected = review.SelectChecks(configured, paths)
	}
	report := review.ChecksReport{Status: "NOT_RUN", Results: []review.CheckResult{}, SideEffectPaths: []string{}}
	if len(selected) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), checkDeadline)
		report = review.RunChecksReport(repoRoot, selected, ctx)
		cancel()
	}
	if len(selected) > 0 {
		results := make([]any, 0, len(report.Results))
		for _, item := range report.Results {
			results = append(results, map[string]any{"name": item.Name, "status": item.Status,
				"exit_code": item.ExitCode, "duration_ms": item.DurationMS})
		}
		bundle["hook_checks"] = map[string]any{"status": report.Status, "results": results}
		bundle["hook_checks_full"] = report
	}
	if len(report.SideEffectPaths) > 0 {
		bundle["findings"] = appendPostFinding(bundle["findings"], "HOOK_CHECK_SIDE_EFFECTS", postJoinFirst(report.SideEffectPaths, 10))
	}
	for _, result := range report.Results {
		if result.Status == "FAIL" || result.Status == "UNKNOWN" {
			code := "HOOK_CHECK_UNKNOWN"
			if result.Status == "FAIL" {
				code = "HOOK_CHECK_FAILED"
			}
			bundle["findings"] = appendPostFinding(bundle["findings"], code, result.Name+": "+result.Tail)
		}
	}

	files, filesOK := pythonInt(delta["changed_file_count"])
	lines, linesOK := pythonInt(delta["changed_line_count"])
	if !filesOK || !linesOK {
		files, lines = -1, -1
	}
	maxFiles, filesOK := pythonInt(gateConfig["small_delta_max_files"])
	maxLines, linesOK := pythonInt(gateConfig["small_delta_max_lines"])
	if !filesOK || !linesOK {
		maxFiles, maxLines = -1, -1
	}
	if resolved.Stored {
		if resolved.SkipMaxFiles != nil && *resolved.SkipMaxFiles < maxFiles {
			maxFiles = *resolved.SkipMaxFiles
		}
		if resolved.SkipMaxLines != nil && *resolved.SkipMaxLines < maxLines {
			maxLines = *resolved.SkipMaxLines
		}
	}

	gate := review.Gate(phase, stringValue(pending["route"]), routeKnown, files, lines, maxFiles, maxLines, reviewerAvailable)
	if files == 0 && lines == 0 {
		gate = review.GateDecision{Reason: review.NoDelta}
	}
	if phase == "plan" {
		gate = review.GateDecision{Reason: review.PhasePlan}
	}
	if resolved.Reviewer == "none" {
		gate = review.GateDecision{Reason: "REVIEWER_DISABLED"}
	}
	if resolved.ForceFullReview {
		bundle["findings"] = appendPostFinding(bundle["findings"], "PROFILE_FORCES_FULL", "The resolved profile forces FULL Claude review.")
	}
	if phase != "plan" && pending["task_contract_grounded"] != true {
		bundle["findings"] = appendPostFinding(bundle["findings"], "TASK_CONTRACT_NOT_GROUNDED", "Task contract is not grounded (missing objective or criteria); forcing FULL Claude review.")
	}
	if gate.Run {
		bundle["reviewer1_skipped_reason"] = nil
		// The reviewer result is not known until pass 98; remain FULL at this boundary.
		bundle["claude_review_mode"] = review.Full
		return
	}
	bundle["reviewer1_result"] = nil
	bundle["reviewer1_skipped_reason"] = gate.Reason

	modeDecision := review.ReviewMode(routeKnown, routeInList(gateConfig["always_full_routes"], stringValue(pending["route"])), gate.Reason, "")
	mode := review.Full
	if gate.Reason == review.NoDelta || gate.Reason == review.BelowThreshold {
		workerValidation, _ := bundle["worker_validation"].(map[string]any)
		validation, _ := workerValidation["status"].(string)
		if report.Status == "PASS" || report.Status == "FAIL" {
			validation = report.Status
		}
		grounded := pending["task_contract_grounded"] == true
		eligible := grounded && validation == "PASS" && delta["status"] == "OK" &&
			!resolved.ForceFullReview && bundle["force_full_review"] != true &&
			len(report.SideEffectPaths) == 0 && len(anyStringList(bundle["protected_paths_touched"])) == 0 &&
			bundle["thread_id"] != nil && routeKnown
		if eligible && modeDecision.Mode == review.Audit {
			mode = review.Audit
		}
	}
	if phase == "plan" || resolved.ForceFullReview || bundle["force_full_review"] == true {
		mode = review.Full
	}
	if phase != "plan" && pending["task_contract_grounded"] != true {
		mode = review.Full
	}
	if bundle["thread_id"] == nil {
		mode = review.Full
	}
	bundle["claude_review_mode"] = mode
}

func routeInList(value any, route string) bool {
	routes, _ := postStringList(value)
	for _, item := range routes {
		if item == route {
			return true
		}
	}
	return false
}

func anyStringList(value any) []string {
	result, _ := postStringList(value)
	return result
}
