package enforce

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
)

func init() {
	observationFields["IMPLEMENTER_FINISHED"] = mergeFields(fields(`duration_ms longest_silent_gap_seconds implementer served_model served_route served_effort served_agent served_provider route phase task_type has_code_map suspected_route_mismatch changed_paths_count known_concurrent_scopes validation worker_validation_status thread_id claude_review_mode routing_enforced codex_usage_known claude_continuation_turn_count reason reason_source previous_route new_route eta_minutes codex_input_tokens codex_cached_input_tokens codex_output_tokens codex_reasoning_output_tokens`), claudeUsagePayloadFields)
	observationFields["MECHANICAL_SHADOW_STARTED"] = fields("")
	observationFields["MECHANICAL_SHADOW_FINISHED"] = fields("success")
	setObservationRule("bool", "success")
	setObservationRule("bool", "has_code_map codex_usage_known")
	setObservationRule("int", "changed_paths_count claude_continuation_turn_count")
	setObservationRule("id", "implementer served_agent served_provider thread_id")
	setObservationRule("model", "served_model")
	setObservationRule("enum:mechanical subtle normal consequential escalated operation plan_standard", "served_route")
	setObservationRule("enum:low medium high xhigh none", "served_effort")
	setObservationRule("enum:PASS FAIL UNKNOWN", "validation worker_validation_status")
	setObservationRule("enum:AUDIT FULL", "claude_review_mode")
	setObservationRule("string_map_list", "known_concurrent_scopes")
	for _, key := range []string{"codex_input_tokens", "codex_cached_input_tokens", "codex_output_tokens", "codex_reasoning_output_tokens"} {
		setObservationRule("number", key)
	}
}

// PostImplementerObservations mirrors the implementer event and task fold at
// the end of Python PostToolUse. The Post handler owns when to call it.
func PostImplementerObservations(event, pending, bundle map[string]any, root string) {
	if agentID := stringValue(pending["dispatcherAgentId"]); agentID != "" {
		event["agent_id"] = agentID
	}
	taskID := pending["task_id"]
	project := pendingProjectKey(pending)
	repository := stringValue(pending["repo_root"])
	var repositoryPtr *string
	if repository != "" {
		repositoryPtr = &repository
	}
	agent := pending["agent"]
	if agent == nil || agent == "" {
		agent = bundle["implementer"]
	}
	if agent == "codex" {
		agent = "codex-main"
	}
	model := bundle["model"]
	if value, ok := model.(string); !ok || !modelValue.MatchString(value) {
		model = pending["model"]
	}
	effort := bundle["effort"]
	if !oneOf(effort, "low medium high xhigh none") {
		effort = pending["effort"]
	}
	usage := postCodexUsage(event["tool_response"])
	implementerName, _ := agent.(string)
	providerName := any(nil)
	if strings.Contains(stringValue(model), "/") {
		providerName = strings.SplitN(stringValue(model), "/", 2)[0]
	} else {
		providerName = provider(implementerName, stringValue(model))
	}
	workerValidation := "UNKNOWN"
	if validation, ok := bundle["worker_validation"].(map[string]any); ok {
		if status, ok := validation["status"].(string); ok {
			workerValidation = status
		}
	}
	threadID := bundle["thread_id"]
	if threadID != nil {
		threadID = normalizeObservationID(threadID, "thread")
	}
	duration := postObservationDuration(event["duration_ms"])
	if jobID := stringValue(mapValue(event["tool_input"])["jobId"]); jobID != "" {
		index, err := readAsyncObject(filepath.Join(root, "async_jobs", jobID+".json"))
		if err == nil {
			if job := asyncJob(root, index); job != nil {
				if elapsed := workerDuration(job["startedAt"], job["finishedAt"]); elapsed > 0 {
					duration = elapsed
				}
			}
		}
	}
	payload := map[string]any{
		"duration_ms": duration, "implementer": agent,
		"served_model": model, "served_effort": effort, "served_route": pending["route"],
		"served_agent": pending["agent"], "served_provider": providerName,
		"route": pending["route"], "phase": pending["phase"],
		"task_type":                pendingValue(pending, "task_type", "unknown"),
		"has_code_map":             pending["has_code_map"] == true,
		"suspected_route_mismatch": pending["suspected_route_mismatch"],
		"changed_paths_count":      bundle["delta_files"],
		"known_concurrent_scopes":  pendingValue(bundle, "known_concurrent_scopes", []any{}),
		"validation":               bundle["validation"], "worker_validation_status": workerValidation,
		"thread_id": threadID, "claude_review_mode": bundle["claude_review_mode"],
		"routing_enforced": bundle["routing_enforced"], "codex_usage_known": len(usage) != 0,
		"claude_continuation_turn_count": pending["claude_continuation_turn_count"],
		"eta_minutes":                    pending["eta_minutes"], "reason": pending["reason"],
		"reason_source": pending["reason_source"], "previous_route": pending["previous_route"],
		"new_route": pending["new_route"],
	}
	if gap, ok := pending["longest_silent_gap_seconds"].(float64); ok && gap >= 0 && !math.IsInf(gap, 0) && !math.IsNaN(gap) {
		payload["longest_silent_gap_seconds"] = gap
	} else if gap, ok := pending["longest_silent_gap_seconds"].(int); ok && gap >= 0 {
		payload["longest_silent_gap_seconds"] = gap
	}
	for key, value := range usage {
		payload["codex_"+key] = value
	}
	preEmit(root, event, repositoryPtr, stringValue(taskID), project, "IMPLEMENTER_FINISHED", payload)
}

func postObservationDuration(value any) any {
	if !validNumber(value, false) {
		return nil
	}
	return numberValue(value)
}

func pendingValue(values map[string]any, key string, fallback any) any {
	if value, ok := values[key]; ok {
		return value
	}
	return fallback
}

func postCodexUsage(response any) map[string]any {
	candidates := []any{response}
	if object, ok := response.(map[string]any); ok {
		candidates = append(candidates, object["structuredContent"])
		if usage, ok := object["usage"].(map[string]any); ok {
			candidates = append(candidates, map[string]any{"usage": usage})
		}
		if content, ok := object["content"].([]any); ok {
			candidates = append(candidates, content...)
		} else {
			candidates = append(candidates, object["content"])
		}
	}
	if list, ok := response.([]any); ok {
		candidates = append(candidates, list...)
	}
	for _, candidate := range candidates {
		var object map[string]any
		switch value := candidate.(type) {
		case map[string]any:
			object = value
		case string:
			_ = json.Unmarshal([]byte(value), &object)
		}
		if text, ok := object["text"].(string); ok {
			var decoded map[string]any
			if json.Unmarshal([]byte(text), &decoded) == nil {
				object = decoded
			}
		}
		usage, ok := object["usage"].(map[string]any)
		if !ok {
			continue
		}
		clean := map[string]any{}
		for _, key := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_output_tokens"} {
			if value := usage[key]; validNumber(value, false) {
				clean[key] = value
			}
		}
		return clean
	}
	return map[string]any{}
}
