package enforce

import (
	"errors"
	"time"

	"purser-hook/internal/review"
)

// recordPostReviewerObservations mirrors codex.post_tool_use's best-effort
// Reviewer-1 event sequence. It stays separate from post.go so the later Post
// tranche can wire it at the same boundary as the Python hook.
func recordPostReviewerObservations(root string, event map[string]any, repository *string, taskID, project string,
	started bool, provider, requestedModel, skippedReason string, result review.BridgeOutcome, bridgeErr error,
	duration time.Duration, initialFindingCount int) {
	if !started {
		if skippedReason != "" {
			preEmit(root, event, repository, taskID, project, "REVIEWER1_SKIPPED", map[string]any{
				"reviewer1_skipped_reason": skippedReason, "reviewer1_model_requested": requestedModel,
				"served_model_attestation": "UNAVAILABLE",
			})
		}
		return
	}
	attrs := result.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	if value, ok := attrs["provider"].(string); ok && value != "" {
		provider = value
	}
	if value, ok := attrs["reviewer1_model_requested"].(string); ok && value != "" {
		requestedModel = value
	}
	preEmit(root, event, repository, taskID, project, "REVIEWER1_STARTED", map[string]any{
		"reviewer1_provider": provider, "reviewer1_model_requested": requestedModel,
	})

	payload := map[string]any{
		"reviewer1_provider": provider, "reviewer1_model_requested": requestedModel,
		"served_model_attestation": "UNAVAILABLE", "duration_ms": float64(duration) / float64(time.Millisecond),
		"gemini_usage_known": false, "reviewer1_retry_count": 0, "reviewer1_retry_recovered": false,
	}
	for _, key := range []string{"reviewer1_result", "reviewer1_retry_count", "reviewer1_retry_recovered"} {
		if value, ok := attrs[key]; ok {
			payload[key] = value
		}
	}
	if value, ok := attrs["result"]; ok {
		payload["reviewer1_result"] = value
	}
	usage, _ := attrs["usage"].(map[string]any)
	if len(usage) > 0 {
		for _, key := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cached_tokens", "cache_read_tokens", "cache_write_tokens", "thinking_tokens"} {
			if value, ok := usage[key]; ok {
				payload["gemini_usage_known"] = true
				payload["gemini_"+key] = value
			}
		}
	}
	if bridgeErr != nil {
		code := "REVIEWER_BRIDGE_EXCEPTION"
		var typed *review.BridgeError
		if errors.As(bridgeErr, &typed) && typed.Code != "" {
			code = typed.Code
		}
		if review.IsReviewerBridgeException(bridgeErr) {
			code = "REVIEWER_BRIDGE_EXCEPTION"
		}
		payload["reviewer1_result"] = "REVIEWER1_UNAVAILABLE"
		payload["reviewer1_error_code"] = code
		payload["failure_code"] = "REVIEWER1_UNAVAILABLE"
	} else if result.Outcome == "completed" {
		payload["reviewer1_result"] = attrs["result"]
		if initialFindingCount >= 0 {
			payload["reviewer1_initial_finding_count"] = initialFindingCount
		}
		preEmit(root, event, repository, taskID, project, "REVIEWER1_FINISHED", payload)
		return
	} else {
		failureCode := "REVIEWER1_INCOMPLETE"
		if result.Outcome == "unavailable" || attrs["result"] == "REVIEWER1_UNAVAILABLE" {
			failureCode = "REVIEWER1_UNAVAILABLE"
		}
		payload["failure_code"] = failureCode
		if code, ok := attrs["error_code"].(string); ok && code != "" {
			payload["reviewer1_error_code"] = code
		} else if findings, ok := attrs["findings"].([]any); ok && len(findings) > 0 {
			if finding, ok := findings[0].(map[string]any); ok {
				payload["reviewer1_error_code"] = finding["code"]
			}
		}
		if payload["reviewer1_error_code"] == nil && failureCode == "REVIEWER1_UNAVAILABLE" {
			payload["reviewer1_error_code"] = failureCode
		}
	}
	preEmit(root, event, repository, taskID, project, "REVIEWER1_FAILED", payload)
}
