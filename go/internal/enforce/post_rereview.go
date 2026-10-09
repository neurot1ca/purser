package enforce

import (
	"fmt"
	"strings"
	"time"

	"purser-hook/internal/review"
)

// rereviewPostRepair repeats Reviewer-1 after a successful micro-repair. The
// shared reviewer bridge owns candidate selection and subprocess execution.
func rereviewPostRepair(root, stateRoot string, pending, bundle map[string]any, resolved postProfile,
	loaded map[string]any, delta, classification map[string]any, started time.Time, budget time.Duration) error {
	outcome, err := runPostReviewer(root, stateRoot, pending, bundle, resolved, loaded, delta, classification, started, budget)
	if err != nil {
		setPostRereview(bundle, review.BridgeOutcome{Outcome: "unavailable"})
		return err
	}
	setPostRereview(bundle, outcome)
	return nil
}

func setPostRereview(bundle map[string]any, outcome review.BridgeOutcome) {
	result := stringValue(outcome.Attributes["result"])
	status := stringValue(outcome.Attributes["review_status"])
	if result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" {
		bundle["micro_repair_cleared_review"] = true
		if repair, ok := bundle["micro_repair"].(map[string]any); ok {
			repair["status"] = "SUCCEEDED"
		}
	} else {
		bundle["micro_repair_cleared_review"] = false
		if repair, ok := bundle["micro_repair"].(map[string]any); ok {
			repair["status"] = "CHANGES_STILL_REQUIRED"
		}
	}
	bundle["reviewer1_result"] = result
	if provider, ok := outcome.Attributes["provider"]; ok {
		bundle["reviewer1_provider"] = provider
	}
	if model, ok := outcome.Attributes["reviewer1_model_requested"]; ok {
		bundle["reviewer1_model_requested"] = model
	}
	validation := stringValue(outcome.Attributes["validation"])
	if validation == "" {
		validation = "UNKNOWN"
	}
	bundle["validation"] = validation
	if status == "UNAVAILABLE" || result == "REVIEWER1_UNAVAILABLE" {
		bundle["reviewer1_skipped_reason"] = "REVIEWER1_UNAVAILABLE"
	}
	if result == "" {
		bundle["reviewer1_result"] = "UNCERTAIN"
	}
	findings, _ := outcome.Attributes["findings"].([]any)
	if findings == nil {
		findings = []any{}
	}
	bundle["findings"] = findings
}

// finalizePostReview chooses the final mode and constructs the audit receipt.
// It mirrors the final eligibility gate after post-repair review and checks.
func finalizePostReview(bundle, pending, loaded map[string]any, gate review.GateDecision,
	classification map[string]any, routeKnown, forceFull, checkSideEffects, profileForcesFull bool) {
	result := stringValue(bundle["reviewer1_result"])
	skipReason := stringValue(bundle["reviewer1_skipped_reason"])
	mode := review.ReviewMode(routeKnown, routeInList(loaded["always_full_routes"], stringValue(pending["route"])), skipReason, result)
	worker := mapValue(bundle["worker_validation"])
	validation := stringValue(worker["status"])
	checks := mapValue(bundle["hook_checks"])
	if hook := stringValue(checks["status"]); hook == "PASS" || hook == "FAIL" {
		validation = hook
	}
	reviewerEligible := result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" ||
		(!gate.Run && (gate.Reason == review.NoDelta || gate.Reason == review.BelowThreshold))
	classificationOK := classification == nil || classification["decision"] == "PROCEED"
	thread := bundle["thread_id"]
	scopePresent := bundle["reviewer1_scope"] != nil
	if scope, ok := bundle["reviewer1_scope"].(map[string]any); ok && len(scope) == 0 {
		scopePresent = false
	}
	eligible := pending["task_contract_grounded"] == true && validation == "PASS" && reviewerEligible &&
		mapValue(bundle["delta"])["status"] == "OK" && classificationOK && !forceFull &&
		!checkSideEffects && !scopePresent && thread != nil && mode.Mode == review.Audit
	if profileForcesFull || stringValue(pending["phase"]) == "plan" || thread == nil || bundle["force_full_review"] == true {
		eligible = false
	}
	if eligible {
		bundle["claude_review_mode"] = review.Audit
		bundle["audit_receipt"] = buildPostAuditReceipt(bundle, stringValue(pending["task_id"]))
		return
	}
	bundle["claude_review_mode"], bundle["audit_receipt"] = review.Full, nil
}

func buildPostAuditReceipt(bundle map[string]any, taskID string) map[string]any {
	paths, ok := postStringList(bundle["changed_paths"])
	if !ok || len(paths) == 0 {
		return nil
	}
	quoted := make([]string, 0, len(paths))
	for _, path := range paths {
		quoted = append(quoted, `"`+path+`"`)
	}
	if taskID == "" {
		taskID = "task"
	}
	return map[string]any{
		"verdict": "READY_FOR_ONE_TURN_ACCEPT", "implementer": bundle["implementer"],
		"model": bundle["model"], "effort": bundle["effort"],
		"reviewer1_verdict":  firstPostValue(bundle["reviewer1_result"], bundle["reviewer1_skipped_reason"]),
		"validation_summary": fmt.Sprintf("validation=%v, delta=%vL/%vF", postDefault(bundle["validation"], "UNKNOWN"), postDefault(bundle["delta_lines"], 0), postDefault(bundle["delta_files"], 0)),
		"commit_command":     fmt.Sprintf("git add %s && git commit -m \"feat(%s): implement changes\"", strings.Join(quoted, " "), taskID),
	}
}

func firstPostValue(primary, fallback any) any {
	if primary != nil && primary != "" {
		return primary
	}
	return fallback
}

func postDefault(value any, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}
