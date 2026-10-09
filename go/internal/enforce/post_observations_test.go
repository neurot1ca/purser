package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPostImplementerObservationsP106(t *testing.T) {
	data, err := os.ReadFile("testdata/post_observations_python_fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
			Line  string         `json:"event_line"`
			Task  map[string]any `json:"task"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			input := c.Input
			pending := input["pending"].(map[string]any)
			bundle := input["bundle"].(map[string]any)
			response := input["response"]
			event := map[string]any{"session_id": "session-post-observations", "agent_id": "agent-post", "agent_type": "worker", "tool_response": response, "duration_ms": input["duration_ms"]}
			if claude, ok := input["claude"].(map[string]any); ok {
				expectedEvent := decodeLine(t, c.Line)
				payload := expectedEvent["payload"].(map[string]any)
				claude["captured_at"] = payload["claude_usage_captured_at"]
				capturedAt, err := time.Parse(time.RFC3339Nano, claude["captured_at"].(string))
				if err != nil {
					t.Fatal(err)
				}
				oldClock := observationNow
				observationNow = func() time.Time { return capturedAt }
				t.Cleanup(func() { observationNow = oldClock })
				data, err := json.Marshal(claude)
				if err != nil {
					t.Fatal(err)
				}
				latest := filepath.Join(root, "telemetry", "latest")
				if err = os.MkdirAll(latest, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(latest, "claude_usage.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			PostImplementerObservations(event, pending, bundle, root)
			paths, err := filepath.Glob(filepath.Join(root, "telemetry", "events", "????-??", "session_*.jsonl"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("event log paths=%v err=%v", paths, err)
			}
			line, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			gotEvent := decodeLine(t, string(line))
			wantEvent := decodeLine(t, c.Line)
			delete(gotEvent, "event_id")
			delete(gotEvent, "ts_utc")
			delete(wantEvent, "event_id")
			delete(wantEvent, "ts_utc")
			if !reflect.DeepEqual(gotEvent, wantEvent) {
				t.Fatalf("event mismatch\ngot:  %#v\nwant: %#v", gotEvent, wantEvent)
			}
			taskPath := filepath.Join(root, "projects", "unresolved", "telemetry", "tasks", stringValue(pending["task_id"])+".json")
			taskData, err := os.ReadFile(taskPath)
			if err != nil {
				t.Fatal(err)
			}
			var gotTask map[string]any
			if err = json.Unmarshal(taskData, &gotTask); err != nil {
				t.Fatal(err)
			}
			wantTask := c.Task
			wantTask["ended_at"] = gotEventTimestamp(t, line)
			if !reflect.DeepEqual(gotTask, wantTask) {
				t.Fatalf("task projection mismatch\ngot:  %#v\nwant: %#v", gotTask, wantTask)
			}
		})
	}
}

func TestPostImplementerObservationRejectsInvalidPayloadP106(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"invalid-served-model":     {"served_model": "bad model"},
		"invalid-concurrent-scope": {"known_concurrent_scopes": []any{map[string]any{"path": "x"}}},
		"unregistered-field":       {"reviewer1_result": "accepted"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildObservation("IMPLEMENTER_FINISHED", map[string]any{}, payload, "task"); err == nil {
				t.Fatal("invalid implementer payload was accepted")
			}
		})
	}
}

func TestPostImplementerObservationsCarriesDispatcherAgentID(t *testing.T) {
	root := t.TempDir()
	event := map[string]any{"session_id": "session-dispatcher", "tool_response": map[string]any{}}
	pending := map[string]any{"task_id": "dispatcher-task", "project_key": "unresolved", "agent": "codex",
		"dispatcherAgentId": "dispatcher-1", "route": "normal", "phase": "implementation", "model": "openai/gpt-5", "effort": "high"}
	bundle := map[string]any{"implementer": "codex-main", "model": "openai/gpt-5", "effort": "high", "validation": "UNKNOWN",
		"worker_validation": map[string]any{"status": "UNKNOWN"}, "claude_review_mode": "FULL", "routing_enforced": true}
	PostImplementerObservations(event, pending, bundle, root)

	data, err := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "dispatcher-task.jsonl"))
	var observation map[string]any
	if err != nil || json.Unmarshal([]byte(strings.TrimSpace(string(data))), &observation) != nil || observation["agent_id"] != "dispatcher-1" {
		t.Fatalf("success observation lost dispatcher identity: %v %s", err, data)
	}
}

func decodeLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(line), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func gotEventTimestamp(t *testing.T, line []byte) any {
	t.Helper()
	return decodeLine(t, string(line))["ts_utc"]
}
