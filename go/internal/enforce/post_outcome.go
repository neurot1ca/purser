package enforce

import (
	"fmt"
	"sort"
	"unicode/utf8"

	"purser-hook/internal/review"
)

// applyPostReviewerOutcome maps the shared bridge response into the PostToolUse
// bundle. The Python hook remains the caller until the post.go migration.
func applyPostReviewerOutcome(root string, event, pending, bundle, delta, classification map[string]any,
	bridge review.BridgeOutcome, routeKnown, alwaysFull bool) {
	attrs := bridge.Attributes
	result := stringValue(attrs["result"])
	status := stringValue(attrs["review_status"])
	if result != "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" && result != "CHANGES_REQUIRED" && result != "UNCERTAIN" && result != "REVIEWER1_UNAVAILABLE" {
		result = "UNCERTAIN"
		bundle["findings"] = appendPostFinding(bundle["findings"], "REVIEWER_RESULT_INVALID", "Reviewer-1 returned an invalid result.")
	}
	if status == "NOT_RUN_INCOMPLETE_PACKET" || bridge.Outcome == "incomplete" {
		result = "UNCERTAIN"
	}
	bundle["reviewer1_result"] = result
	if status == "UNAVAILABLE" || result == "REVIEWER1_UNAVAILABLE" {
		if bundle["reviewer1_skipped_reason"] == nil {
			bundle["reviewer1_skipped_reason"] = "REVIEWER1_UNAVAILABLE"
		}
	}
	for target, source := range map[string]string{
		"reviewer1_provider": "provider", "reviewer1_model_requested": "reviewer1_model_requested",
		"served_model_attestation": "served_model_attestation",
	} {
		if attrs[source] != nil {
			bundle[target] = attrs[source]
		}
	}
	validation := stringValue(attrs["validation"])
	if validation != "PASS" && validation != "FAIL" && validation != "UNKNOWN" {
		validation = "UNKNOWN"
	}
	bundle["validation"] = validation

	findings := postReviewerFindings(attrs["findings"])
	if code := stringValue(attrs["error_code"]); code == "REVIEWER_RESPONSE_INVALID" {
		message := "Reviewer-1 response was invalid."
		if values := postReviewerFindings(attrs["findings"]); len(values) > 0 {
			message = values[0]["message"].(string)
		}
		if raw := attrs["raw_stdout"]; raw != nil {
			if path, err := persistRawOutputAs(root, reviewerOutputName(event), raw); err == nil {
				message += "; raw output: " + path
			} else {
				message += fmt.Sprintf(" raw output not saved (%T)", err)
			}
			if detail := stringValue(attrs["decoder_detail"]); detail != "" {
				message += "; decoder detail: " + runePreview(detail, 200)
			}
		}
		findings = []map[string]any{{"code": code, "message": message}}
	}
	for _, finding := range findings {
		bundle["findings"] = appendPostFinding(bundle["findings"], finding["code"].(string), finding["message"].(string))
	}

	jobPaths, ok := postStringList(mapValue(bundle["whole_tree_delta"])["changed_paths"])
	if !ok {
		jobPaths, _ = postStringList(delta["changed_paths"])
	}
	if jobPaths != nil {
		var reviewed []string
		reason := "Some job delta paths were excluded from Reviewer-1's packet."
		if status == "NOT_RUN_INCOMPLETE_PACKET" || bridge.Outcome == "incomplete" {
			reason = "Reviewer-1 did not receive a complete packet."
		} else {
			if classified, valid := classification["in_scope_paths"]; valid {
				if scopePaths, valid := postStringList(classified); valid {
					reviewed = scopePaths
					if deltaPaths, valid := postStringList(delta["changed_paths"]); valid && !sameStringSet(reviewed, deltaPaths) {
						reason = "Only delegated in-scope paths were sent to Reviewer-1."
					}
				}
			} else if paths, valid := postStringList(delta["changed_paths"]); valid {
				reviewed = paths
			}
		}
		omitted := differenceSorted(jobPaths, reviewed)
		if len(omitted) > 0 {
			bundle["reviewer1_scope"] = map[string]any{"reviewed": postPathPreview(reviewed), "omitted": postPathPreview(omitted), "reason": reason}
		}
	}

	skip := stringValue(bundle["reviewer1_skipped_reason"])
	decision := review.ReviewMode(routeKnown, alwaysFull, skip, result)
	mode := decision.Mode
	worker := mapValue(bundle["worker_validation"])
	validationStatus := stringValue(worker["status"])
	classificationProceed := classification == nil || stringValue(classification["decision"]) == "PROCEED"
	auditEligible := pending["task_contract_grounded"] == true && validationStatus == "PASS" &&
		(result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" || skip == review.NoDelta || skip == review.BelowThreshold) &&
		delta["status"] == "OK" && classificationProceed && pending["force_full_review"] != true &&
		bundle["force_full_review"] != true && len(anyStringList(bundle["protected_paths_touched"])) == 0 &&
		bundle["reviewer1_scope"] == nil && bundle["thread_id"] != nil && routeKnown && !alwaysFull &&
		stringValue(pending["phase"]) != "plan"
	if !auditEligible || mode != review.Audit {
		mode = review.Full
	}
	bundle["claude_review_mode"] = mode
}

func reviewerOutputName(event map[string]any) string {
	key, err := callKey(event)
	if err != nil {
		return "reviewer1-invalid.json"
	}
	return key + "-reviewer1.json"
}

func runePreview(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}

func postReviewerFindings(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, min(len(items), 20))
	for _, raw := range items {
		finding := mapValue(raw)
		code, message := stringValue(finding["code"]), stringValue(finding["message"])
		if code == "" || message == "" || utf8.RuneCountInString(code) > 120 {
			continue
		}
		result = append(result, map[string]any{"code": code, "message": message})
		if len(result) == 20 {
			break
		}
	}
	return result
}

func postPathPreview(paths []string) []string {
	result := append([]string{}, paths[:min(len(paths), 10)]...)
	if len(paths) > 10 {
		result = append(result, fmt.Sprintf("+%d more", len(paths)-10))
	}
	return result
}

func differenceSorted(all, included []string) []string {
	set := make(map[string]bool, len(included))
	for _, value := range included {
		set[value] = true
	}
	var result []string
	for _, value := range all {
		if !set[value] {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	return len(differenceSorted(left, right)) == 0 && len(differenceSorted(right, left)) == 0
}
