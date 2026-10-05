package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/lease"
)

func TestPostIntakeMatchesProcessOracle(t *testing.T) {
	data, _ := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_process.json"))
	var fixture struct {
		Cases []map[string]any `json:"cases"`
	}
	mustPostTest(t, json.Unmarshal(data, &fixture))
	pluginRoot, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	for _, name := range []string{"bypass_unrouted", "undelivered_marker"} {
		var oracle map[string]any
		for _, candidate := range fixture.Cases {
			if candidate["name"] == name {
				oracle = candidate
				break
			}
		}
		if oracle == nil {
			t.Fatal("oracle case missing")
		}
		root := t.TempDir()
		event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex",
			"tool_use_id": name, "session_id": "p93-session", "tool_input": map[string]any{"label": name},
			"tool_response": map[string]any{"structuredContent": map[string]any{"threadId": "post-thread", "content": "Done."}}}
		key, _ := callKey(event)
		pending := map[string]any{"kind": "pretool_bypass", "reason": "pretool config error"}
		pendingPath, _ := pendingPath(root, event)
		if name == "bypass_unrouted" {
			pendingPath = bypassPath(root, event)
		}
		if name == "undelivered_marker" {
			repository := filepath.Join(root, "repo")
			project := projectKey(&repository)
			pending = map[string]any{"kind": "call", "task_id": "post-marker", "phase": "implementation",
				"project_key": project, "lease_owner": "owner-p93"}
			if got := lease.New(filepath.Join(root, "projects", project)).Acquire("owner-p93", "post-marker", []string{"changed.py"}, lease.AcquireOptions{}, 100); got.Status != "OK" {
				t.Fatalf("lease setup: %+v", got)
			}
			marker := filepath.Join(root, "pending", "undelivered-"+key+".json")
			mustPostTest(t, os.MkdirAll(filepath.Dir(marker), 0700))
			mustPostTest(t, os.WriteFile(marker, []byte(`{"reason":"rewrite undelivered"}`), 0600))
		}
		mustPostTest(t, os.MkdirAll(filepath.Dir(pendingPath), 0700))
		pendingData, _ := json.Marshal(pending)
		mustPostTest(t, os.WriteFile(pendingPath, pendingData, 0600))
		_, early, err := postIntake(event, root)
		mustPostTest(t, err)
		response := mapValue(early["hookSpecificOutput"])
		outputs := response["updatedToolOutput"].([]any)
		var bundle map[string]any
		mustPostTest(t, json.Unmarshal([]byte(outputs[0].(map[string]any)["text"].(string)), &bundle))
		wantBundle := oracle["bundle"].(map[string]any)
		for key, want := range wantBundle {
			if !reflect.DeepEqual(bundle[key], want) {
				t.Errorf("bundle[%s] = %#v, want %#v", key, bundle[key], want)
			}
		}
		var findings []string
		for _, raw := range bundle["findings"].([]any) {
			findings = append(findings, raw.(map[string]any)["code"].(string))
		}
		wantFindings := []string{oracle["findings"].([]any)[0].(string)}
		if !reflect.DeepEqual(findings, wantFindings) {
			t.Errorf("finding codes = %v, want %v", findings, oracle["findings"])
		}
		if remaining, _ := os.ReadDir(filepath.Join(root, "pending")); len(remaining) != 0 {
			t.Errorf("pending remaining: %v", remaining)
		}
		if name == "undelivered_marker" {
			leaseData, readErr := os.ReadFile(filepath.Join(root, "projects", stringValue(pending["project_key"]), "scope_leases.json"))
			if readErr != nil || !bytes.Contains(leaseData, []byte(`"state":"RELEASED"`)) {
				t.Fatalf("lease not released: %s %v", leaseData, readErr)
			}
			events, err := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "post-marker.jsonl"))
			if err != nil {
				t.Fatalf("routing violation observation missing: %v", err)
			}
			if !bytes.Contains(events, []byte(`"event_type":"ROUTING_PROTOCOL_VIOLATION"`)) {
				t.Fatalf("unexpected observation: %s", events)
			}
		}
	}
}
func TestPostIntakeErrorsAndProfiles(t *testing.T) {
	root := t.TempDir()
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex", "tool_use_id": "p93", "session_id": "session", "tool_input": map[string]any{}}
	paths := pendingCandidates(root, event)
	if _, _, err := postIntake(event, root); err == nil {
		t.Fatal("missing pending record accepted")
	}
	mustPostTest(t, os.MkdirAll(filepath.Dir(paths[0]), 0700))
	mustPostTest(t, os.WriteFile(paths[0], []byte("{"), 0600))
	if _, _, err := postIntake(event, root); err == nil {
		t.Fatal("malformed pending record accepted")
	}
	if _, early, err := postIntake(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "other"}, root); err != nil || early == nil {
		t.Fatal("non-Codex event was not returned early")
	}
	defaultProfile := postProfileFromPending(map[string]any{}, map[string]any{"reviewer1_model_requested": "luna"})
	storedProfile := postProfileFromPending(map[string]any{"protected_paths": []any{"secret"}, "force_full_review": false, "skip_max_lines": float64(5), "skip_max_files": float64(2), "configured": true, "user_status": "accepted", "project_status": "accepted", "reviewer_candidates": []any{}}, map[string]any{"reviewer1_model_requested": "luna"})
	if defaultProfile.Stored || !defaultProfile.ForceFullReview || !storedProfile.Stored || storedProfile.ForceFullReview {
		t.Fatalf("profiles: default=%+v stored=%+v", defaultProfile, storedProfile)
	}
}

func mustPostTest(t *testing.T, err error) {
	if err != nil {
		t.Fatal(err)
	}
}
