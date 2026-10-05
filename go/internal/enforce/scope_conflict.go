package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"

	"purser-hook/internal/lease"
)

// RecordScopeConflictOutcome persists the best-effort telemetry emitted by the bridge.
func RecordScopeConflictOutcome(stateRoot string, job map[string]any, action string) {
	defer func() { _ = recover() }()
	jobID, ok := job["jobId"].(string)
	if !ok || jobID == "" {
		return
	}
	taskID := job["taskId"]
	var project any
	if repo, ok := job["repoRoot"].(string); ok && repo != "" {
		project = projectKey(&repo)
	}
	var queueWait any
	if projectKeyValue, ok := project.(string); ok {
		raw, err := os.ReadFile(filepath.Join(stateRoot, "async_jobs", jobID+".json"))
		var index map[string]any
		if err == nil && json.Unmarshal(raw, &index) == nil && index["jobId"] == jobID && index["project_key"] == projectKeyValue {
			if owner, ok := index["call_key"].(string); ok && owner != "" {
				queueWait = lease.New(filepath.Join(stateRoot, "projects", projectKeyValue)).QueueWaitMS(owner)
			}
		}
	}
	envelope := map[string]any{"session_id": nil, "project_key": project, "repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil}
	payload := map[string]any{"action": action, "job_id": jobID, "queue_wait_ms": queueWait}
	if event, err := buildObservationAtStateRoot("SCOPE_CONFLICT_OUTCOME", envelope, payload, taskID, stateRoot); err == nil {
		_, _ = persistObservation(filepath.Join(stateRoot, "telemetry"), event)
	}
}
