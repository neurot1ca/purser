package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/lease"
)

func TestFinishPostSessionPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_session.json")
	mustPostTest(t, err)
	var fixture struct {
		Cases []struct {
			Name       string         `json:"name"`
			Pending    bool           `json:"pending"`
			Session    map[string]any `json:"session"`
			Lease      map[string]any `json:"lease"`
			Thread     map[string]any `json:"thread"`
			Bundle     map[string]any `json:"bundle"`
			Repository string         `json:"repository"`
		} `json:"cases"`
	}
	mustPostTest(t, json.Unmarshal(data, &fixture))
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			mustPostTest(t, os.Mkdir(repo, 0700))
			pending := map[string]any{"task_id": "session-" + test.Name, "project_key": "fixture-project",
				"route": "operation", "model": "gpt-6-luna", "effort": "high", "phase": "implementation",
				"scope": []any{}, "agent": "codex", "task_contract_grounded": false}
			if test.Name == "sync_success_releases" || test.Name == "worktree_success_retains" || test.Name == "ledger_save_failure" || test.Name == "pending_already_gone" {
				pending["lease_owner"] = "owner-" + test.Name
			}
			if test.Name == "plan_without_lease" {
				pending["phase"] = "plan"
			}
			if test.Name == "pending_already_gone" {
				pending["phase"] = "operation"
			}
			if test.Name == "bypass_without_routing" {
				pending["kind"] = "pretool_bypass"
				pending["route"], pending["model"], pending["effort"] = "", "", ""
			}
			if test.Name == "worktree_success_retains" {
				pending["worktree_path"] = repo
			}
			event := map[string]any{"tool_use_id": "call-" + test.Name, "session_id": "session-p109"}
			key, err := callKey(event)
			mustPostTest(t, err)
			pendingFile := filepath.Join(root, "pending", key+".json")
			mustPostTest(t, os.MkdirAll(filepath.Dir(pendingFile), 0700))
			if test.Name != "pending_already_gone" {
				encoded, marshalErr := json.Marshal(pending)
				mustPostTest(t, marshalErr)
				mustPostTest(t, os.WriteFile(pendingFile, encoded, 0600))
			}
			projectState := filepath.Join(root, "projects", "fixture-project")
			if test.Name == "sync_success_releases" || test.Name == "worktree_success_retains" || test.Name == "ledger_save_failure" || test.Name == "pending_already_gone" {
				got := lease.New(projectState).Acquire(stringValue(pending["lease_owner"]), stringValue(pending["task_id"]), []string{"src"}, lease.AcquireOptions{}, 1700000000)
				if got.Status != "OK" {
					t.Fatalf("lease setup: %+v", got)
				}
			}
			if test.Name == "ledger_save_failure" {
				mustPostTest(t, os.WriteFile(filepath.Join(projectState, "sessions"), []byte("blocked"), 0600))
			}
			bundle := map[string]any{"findings": []any{}, "claude_review_mode": "AUDIT", "status": "OK"}
			unapplied := test.Name == "worktree_success_retains"
			if err := finishPostSession(root, event, pending, bundle, "thread-p109", repo, unapplied, 1700000000); err != nil {
				t.Fatal(err)
			}
			var session, leaseState, thread map[string]any
			if path, err := filepath.Glob(filepath.Join(projectState, "sessions", "*.json")); err != nil {
				t.Fatal(err)
			} else if len(path) != 0 {
				session = readPostSessionJSON(t, path[0])
			}
			leasePath := filepath.Join(projectState, "scope_leases.json")
			if _, err := os.Stat(leasePath); err == nil {
				leaseState = readPostSessionJSON(t, leasePath)
			}
			if path, err := filepath.Glob(filepath.Join(root, "threads", "*.json")); err != nil {
				t.Fatal(err)
			} else if len(path) != 0 {
				thread = readPostSessionJSON(t, path[0])
			}
			if session != nil {
				if payload, ok := session["payload"].(map[string]any); ok {
					if outcome, ok := payload["last_outcome"].(map[string]any); ok {
						outcome["findings"] = []any{}
					}
				}
			}
			if thread != nil {
				thread["project_root"] = "__REPO__"
			}
			got := map[string]any{"name": test.Name, "pending": fileExists(pendingFile), "session": session,
				"lease": leaseState, "thread": thread, "bundle": bundle, "repository": "__REPO__"}
			want := map[string]any{"name": test.Name, "pending": test.Pending, "session": test.Session,
				"lease": test.Lease, "thread": test.Thread, "bundle": test.Bundle, "repository": test.Repository}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("post session state mismatch\ngot:  %#v\nwant: %#v", got, want)
			}
		})
	}
}

func readPostSessionJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	mustPostTest(t, err)
	var value map[string]any
	mustPostTest(t, json.Unmarshal(data, &value))
	return value
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
