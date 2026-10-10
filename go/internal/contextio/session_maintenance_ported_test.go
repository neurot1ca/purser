package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSessionMaintenanceJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionDurationHintPythonParity(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(root, "telemetry", "events", now.Format("2006-01"), "session_duration.jsonl")
	var lines []string
	add := func(agent string, minutes int) {
		t.Helper()
		payload := map[string]any{"route": "mechanical", "duration_ms": minutes * 60000}
		if agent != "" {
			payload["implementer"] = agent
		}
		item := map[string]any{"event_type": "IMPLEMENTER_FINISHED", "project_key": "project",
			"ts_utc": now.Format(time.RFC3339), "payload": payload}
		data, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	for i := 0; i < 4; i++ {
		add("codex-main", 2)
	}
	add("", 8)
	for i := 0; i < 5; i++ {
		add("claude", 10)
	}
	for i := 0; i < 3; i++ {
		add("codex", 2)
	}
	for i := 0; i < 2; i++ {
		add("codex-main", 8)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}

	if got := durationHint(root, "project", "codex-main"); !strings.Contains(got, "mechanical 2/8 min (n=9)") {
		t.Fatalf("Codex duration hint = %q", got)
	}
	if got := durationHint(root, "project", "claude"); !strings.Contains(got, "mechanical 10/10 min (n=5)") {
		t.Fatalf("Claude duration hint = %q", got)
	}
	if got := durationHint(root, "project", "other"); !strings.Contains(got, "mechanical 8/10 min (n=15)") {
		t.Fatalf("pooled duration hint = %q", got)
	}
}

func TestSessionCompactAccuracyHintPythonParity(t *testing.T) {
	root := t.TempDir()
	project := "project"
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(root, "telemetry", "events", now.Format("2006-01"), "session_accuracy.jsonl")
	item := map[string]any{"event_type": "IMPLEMENTER_FINISHED", "project_key": project,
		"ts_utc": now.Format(time.RFC3339), "payload": map[string]any{
			"implementer": "claude", "route": "normal", "duration_ms": 120000, "eta_minutes": 3,
		}}
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "projects", project, "duration_accuracy.json")
	writeSessionMaintenanceJSON(t, statePath, map[string]any{"compacts": 4, "errors": map[string]any{"claude": 75}})
	if got := compactAccuracyHint(root, project); !strings.Contains(got, "Claude's task duration estimates have improved") || !strings.Contains(got, "50%") {
		t.Fatalf("accuracy improvement hint = %q", got)
	}
	if got := compactAccuracyHint(root, project); got != "" {
		t.Fatalf("accuracy hint repeated before next five compactions: %q", got)
	}
	var state map[string]any
	stateData, err := os.ReadFile(statePath)
	if err != nil || json.Unmarshal(stateData, &state) != nil {
		t.Fatalf("read accuracy state: %v", err)
	}
	if state["compacts"] != float64(6) {
		t.Fatalf("compact count = %v, want 6", state["compacts"])
	}
	item["payload"].(map[string]any)["implementer"] = "agent-x"
	unknownData, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(unknownData, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	writeSessionMaintenanceJSON(t, statePath, map[string]any{"compacts": 4, "errors": map[string]any{"agent-x": 75}})
	if got := compactAccuracyHint(root, project); !strings.Contains(got, "agent-x's task duration estimates have improved") {
		t.Fatalf("unknown implementer possessive hint = %q", got)
	}
}

func TestFollowupHintsTerminalPythonParity(t *testing.T) {
	root := t.TempDir()
	const project, session, tool, call = "project", "session-1", "tool-1", "failed-call"
	jobID := sha256.Sum256([]byte(call))
	id := hex.EncodeToString(jobID[:])
	callKeySum := sha256.Sum256([]byte(session + "\x00" + tool))
	callKey := hex.EncodeToString(callKeySum[:])
	indexPath := filepath.Join(root, "async_jobs", id+".json")
	pendingPath := filepath.Join(root, "pending", callKey+".json")
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
	write := func(state string, observed bool) {
		t.Helper()
		writeSessionMaintenanceJSON(t, indexPath, map[string]any{"jobId": id, "call": call, "call_key": callKey,
			"session_id": session, "project_key": project})
		writeSessionMaintenanceJSON(t, pendingPath, map[string]any{"job_id": id, "async_call": call,
			"session_id": session, "project_key": project, "tool_use_id": tool, "task_id": "failed task"})
		job := map[string]any{"jobId": id, "call": call, "state": state}
		if observed {
			job["waiterObservedTerminalAt"] = time.Now().Unix()
			job["waiterObservedTerminalState"] = state
		}
		writeSessionMaintenanceJSON(t, jobPath, job)
	}

	write("FAILED", true)
	if got := followupHints(root, project, session); got != "" {
		t.Fatalf("observed failure in same session produced hint: %q", got)
	}
	if got := followupHints(root, project, "session-2"); !strings.Contains(got, "adopt: true") {
		t.Fatalf("observed failure in other session hint = %q", got)
	}
	write("FAILED", false)
	if got := followupHints(root, project, session); !strings.Contains(got, "Call codex_result") {
		t.Fatalf("unobserved failure hint = %q", got)
	}
	write("SUCCEEDED", true)
	if got := followupHints(root, project, session); !strings.Contains(got, "Call codex_result") {
		t.Fatalf("observed success hint = %q", got)
	}
	write("UNKNOWN_OR_IN_PROGRESS", false)
	if got := followupHints(root, project, session); !strings.Contains(got, "Call codex_result") {
		t.Fatalf("same-session unknown hint = %q", got)
	}
	if got := followupHints(root, project, "session-2"); got != "" {
		t.Fatalf("other-session unknown hint = %q", got)
	}
	write("RUNNING", false)
	writeSessionMaintenanceJSON(t, jobPath, map[string]any{"jobId": id, "call": call, "state": "RUNNING",
		"errorCode": "STALLED", "stallDetectedAt": time.Now().Unix(),
		"liveness": map[string]any{"lastActivityAt": time.Now().Add(-125 * time.Second).Unix()}})
	got := followupHints(root, project, session)
	if !strings.Contains(strings.ToLower(got), "do not call codex_result") ||
		!strings.Contains(got, "Restart ") || !strings.Contains(got, " wait "+id) ||
		!strings.Contains(got, fmt.Sprintf("%d minutes", 2)) {
		t.Fatalf("stalled followup hint = %q", got)
	}
}
