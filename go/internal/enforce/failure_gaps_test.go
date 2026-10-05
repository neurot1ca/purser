package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFailureGapsPythonFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "failure_gaps.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]map[string]any
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for name, want := range fixture {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			pending := map[string]any{"task_id": "task-" + name, "project_key": "0123456789abcdef",
				"route": "normal", "model": "model-a", "effort": "high", "agent": "opencode",
				"mechanical_opencode": true}
			event := map[string]any{"tool_name": "mcp__codex-main__codex", "tool_use_id": name,
				"session_id": "failure-gaps", "hook_event_name": "PostToolUseFailure"}
			got := map[string]any{}
			switch name {
			case "undelivered_marker":
				key, err := callKey(event)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "pending", "undelivered-"+key+".json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"reason":"adapter did not deliver"}`), 0600); err != nil {
					t.Fatal(err)
				}
				marker, found := failureUndelivered(root, event, pending)
				if !found {
					t.Fatal("undelivered marker not found")
				}
				failureUndeliveredTail(root, marker, "", event, pending)
				_, markerErr := os.Stat(path)
				observations, _ := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "task-undelivered_marker.jsonl"))
				got = map[string]any{"kind": pending["kind"], "reason": pending["reason"],
					"marker_exists": !os.IsNotExist(markerErr), "protocol_violation": string(observations) != ""}
			case "mechanical_negative":
				writeInventory(t, root, map[string]any{"candidates": []any{map[string]any{"id": "model-a", "status": "available", "mechanical_samples": []any{true}}}})
				recordFailedMechanicalAttempt(root, pending)
				var inventory map[string]any
				data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
				if err != nil || json.Unmarshal(data, &inventory) != nil {
					t.Fatalf("read mechanical inventory: %v", err)
				}
				for _, raw := range inventory["candidates"].([]any) {
					candidate := raw.(map[string]any)
					if candidate["id"] == pending["model"] {
						got["samples"] = candidate["mechanical_samples"]
					}
				}
			case "internal_diagnostic":
				response := failureInternalResponse(root, event, pending, os.ErrPermission)
				data, err := os.ReadFile(filepath.Join(root, "diagnostics.jsonl"))
				if err != nil {
					t.Fatalf("diagnostic missing: %v", err)
				}
				var diagnostic map[string]any
				if err := json.Unmarshal(data[:len(data)-1], &diagnostic); err != nil {
					t.Fatal(err)
				}
				bundle := readReplacementBundle(t, response)
				got = map[string]any{"phase": diagnostic["phase"],
					"finding": mapValue(bundle["findings"].([]any)[0])["code"], "review_mode": bundle["claude_review_mode"]}
			case "task_projection":
				failureObservation(root, event, pending)
				failureObservationTail(root, pending)
				path := filepath.Join(root, "projects", pending["project_key"].(string), "telemetry", "tasks", pending["task_id"].(string)+".json")
				_, err := os.Stat(path)
				got["projected"] = err == nil
			case "observation_failure_tolerated":
				if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				preEmit(filepath.Join(root, "blocker"), event, nil, "", "unresolved", "ROUTING_PROTOCOL_VIOLATION", map[string]any{"violation_code": "CODEX_INPUT_REWRITE_UNDELIVERED"})
				got["returned"] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("result = %#v, want %#v", got, want)
			}
		})
	}
}

func writeInventory(t *testing.T, root string, inventory map[string]any) {
	t.Helper()
	path := filepath.Join(root, "opencode", "candidates.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readReplacementBundle(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	ctx := mapValue(response["hookSpecificOutput"])
	items, _ := ctx["updatedToolOutput"].([]any)
	if len(items) != 1 {
		t.Fatalf("replacement output = %#v", ctx)
	}
	item := mapValue(items[0])
	var bundle map[string]any
	if err := json.Unmarshal([]byte(stringValue(item["text"])), &bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}
