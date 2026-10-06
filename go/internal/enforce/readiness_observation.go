package enforce

import "path/filepath"

// RecordReadinessOutcome mirrors the Python host's best-effort telemetry.
func RecordReadinessOutcome(stateRoot string, job map[string]any, verdict, agent string, durationMS int64) {
	defer func() { _ = recover() }()
	var project any
	if repo, ok := job["repoRoot"].(string); ok && repo != "" {
		project = projectKey(&repo)
	}
	envelope := map[string]any{"session_id": nil, "project_key": project, "repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil}
	payload := map[string]any{"verdict": verdict, "agent": agent, "duration_ms": durationMS}
	if event, err := buildObservationAtStateRoot("READINESS_CHECK_OUTCOME", envelope, payload, job["taskId"], stateRoot); err == nil {
		_, _ = persistObservation(filepath.Join(stateRoot, "telemetry"), event)
	}
}
