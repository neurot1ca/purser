package enforce

import (
	"fmt"
	"path/filepath"

	"purser-hook/internal/ledger"
)

func failureEvidence(event, pending map[string]any) map[string]any {
	return map[string]any{"ambiguous_outcome": true, "work_performed": "UNKNOWN",
		"error": event["error"], "is_interrupt": event["is_interrupt"],
		"duration_ms": event["duration_ms"], "pending_call": pending}
}
func persistFailureEvidence(root, key string, evidence map[string]any) (string, error) {
	return persistRawOutputAs(root, key+"-failure.json", evidence)
}
func saveFailureSession(root string, event, pending map[string]any, outcome map[string]any) error {
	taskID := stringValue(pending["task_id"])
	if taskID != "" {
		taskID = safeEventID(taskID)
	}
	threadID := stringValue(pending["prior_thread_id"])
	if event["error_code"] == "JOB_REPLAY_PREPARATION_FAILED" {
		threadID = stringValue(event["replay_thread_id"])
	}
	var thread *string
	if threadID != "" {
		thread = &threadID
	}
	options := map[string]any{"phase": pending["phase"],
		"scope": pending["scope"], "reason": pending["reason"], "label": pending["label"],
		"task_objective": pending["task_objective"], "acceptance_criteria": pending["acceptance_criteria"],
		"task_contract_grounded": pending["task_contract_grounded"] == true,
		"pending_call_baseline":  nil, "last_outcome": outcome}
	if agent := stringValue(pending["agent"]); agent != "" {
		options["agent"] = agent
	}
	// Preserve the established sync record shape for implementation calls without scope.
	if options["phase"] == "implementation" {
		if scope, ok := options["scope"].([]any); !ok || len(scope) == 0 {
			delete(options, "phase")
			delete(options, "scope")
		}
	}
	record := ledger.BuildSessionRecord(taskID, thread, stringValue(pending["route"]),
		stringValue(pending["model"]), stringValue(pending["effort"]), options)
	projectKey := stringValue(pending["project_key"])
	if projectKey == "" {
		projectKey = "unresolved"
	}
	if saved := ledger.Save(filepath.Join(root, "projects", projectKey), record); saved.Status != ledger.OK {
		return fmt.Errorf("ambiguous failure could not be recorded: %s", saved.Code)
	}
	return nil
}
func failureObservation(root string, event, pending map[string]any) {
	defer func() { _ = recover() }()
	taskID := stringValue(pending["task_id"])
	projectKey := stringValue(pending["project_key"])
	if projectKey == "" {
		projectKey = "unresolved"
	}
	code := "CODEX_CALL_FAILED"
	if event["is_interrupt"] == true {
		code = "CODEX_CALL_INTERRUPTED"
	}
	payload := map[string]any{"failure_code": code, "duration_ms": telemetryDuration(event["duration_ms"]),
		"eta_minutes": pending["eta_minutes"], "reason": pending["reason"], "reason_source": pending["reason_source"],
		"previous_route": pending["previous_route"], "new_route": pending["new_route"]}
	if gap, ok := pending["longest_silent_gap_seconds"].(float64); ok {
		payload["longest_silent_gap_seconds"] = gap
	} else if gap, ok := pending["longest_silent_gap_seconds"].(int); ok {
		payload["longest_silent_gap_seconds"] = gap
	}
	var repository *string
	if path := stringValue(pending["repo_root"]); path != "" {
		repository = &path
	}
	preEmit(root, event, repository, taskID, projectKey, "IMPLEMENTER_FAILED", payload)
}
