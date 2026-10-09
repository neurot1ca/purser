package enforce

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func cleanupStaleFailurePending(root string, event map[string]any, key string) {
	defer func() { _ = recover() }()
	current := map[string]bool{filepath.Join(root, "pending", key+".json"): true,
		bypassPath(root, event): true}
	paths, err := filepath.Glob(filepath.Join(root, "pending", "*.json"))
	if err != nil {
		return
	}
	for i, path := range paths {
		if i >= 32 {
			break
		}
		if current[path] {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || time.Since(info.ModTime()) <= 24*time.Hour {
			continue
		}
		data, err := os.ReadFile(path)
		var old map[string]any
		if err != nil || json.Unmarshal(data, &old) != nil || old == nil {
			continue
		}
		_, hasLeaseOwner := old["lease_owner"].(string)
		_, hasAsyncCall := old["async_call"].(string)
		_, hasJobID := old["job_id"].(string)
		if hasLeaseOwner || hasAsyncCall || hasJobID {
			continue
		}
		_ = os.Remove(path)
	}
}

func cleanupFailureTail(root, pendingFile string, event, pending map[string]any, removeJob bool) error {
	projectKey := stringValue(pending["project_key"])
	if projectKey == "" {
		projectKey = "unresolved"
	}
	if owner, ok := pending["lease_owner"].(string); ok {
		result := leaseRelease(root, projectKey, owner)
		if result != nil {
			return result
		}
	}
	if err := os.Remove(pendingFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if removeJob && strings.HasSuffix(stringValue(event["tool_name"]), "__codex_start") {
		jobID := stringValue(pending["job_id"])
		if asyncHash.MatchString(jobID) {
			if err := os.Remove(filepath.Join(root, "async_jobs", jobID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("async job cleanup failed: %w", err)
			}
		}
	}
	return nil
}

func failureSuccessOutput(evidencePath string, routingEnforced bool) map[string]any {
	note := ""
	if !routingEnforced {
		note = " Routing was NOT enforced by PreToolUse."
	}
	ctx := map[string]any{"hookEventName": "PostToolUseFailure",
		"additionalContext": fmt.Sprintf("Codex failed with an ambiguous outcome; work_performed is UNKNOWN, FULL Claude review is required, and evidence is at %s.%s", evidencePath, note)}
	return map[string]any{"hookSpecificOutput": ctx}
}

func failureErrorOutput(message string) map[string]any {
	ctx := map[string]any{"hookEventName": "PostToolUseFailure",
		"additionalContext": "Codex failed with an ambiguous outcome; work_performed is UNKNOWN and FULL Claude review is required. Evidence cleanup error: " + message}
	return map[string]any{"hookSpecificOutput": ctx}
}

func failureErrorText(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		kind := "OSError"
		if errors.Is(pathErr.Err, os.ErrPermission) {
			kind = "PermissionError"
		}
		return kind + ": " + pathErr.Err.Error()
	}
	return "WiringError: " + err.Error()
}

func leaseRelease(root, projectKey, owner string) error {
	result := lease.New(filepath.Join(root, "projects", projectKey)).Release(owner, float64(time.Now().UnixNano())/1e9)
	if result.Status != "OK" {
		return fmt.Errorf("%s", result.Code)
	}
	return nil
}

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
	if agentID := stringValue(pending["dispatcherAgentId"]); agentID != "" {
		event["agent_id"] = agentID
	}
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
