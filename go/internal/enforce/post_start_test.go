package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/lease"
)

func TestPostStartPythonWriteFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_start.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name        string         `json:"name"`
			Output      map[string]any `json:"output"`
			Index       map[string]any `json:"index"`
			Pending     map[string]any `json:"pending"`
			LeaseStates []string       `json:"lease_states"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "codex._post_start+post_tool_use_failure" || len(fixture.Cases) != 5 {
		t.Fatalf("unexpected Post start fixture: %+v", fixture)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			project := "fixture-project"
			toolID := "start-" + tc.Name
			event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_start",
				"tool_use_id": toolID, "session_id": "post-start-fixture-session"}
			call := "fixture-call-" + tc.Name
			jobID := jobHash(call)
			owner, _ := callKey(event)
			pending := map[string]any{"async_call": call, "async_ready": true, "session_id": event["session_id"],
				"tool_use_id": toolID, "project_key": project, "repo_root": repo, "lease_owner": owner,
				"task_id": "post-start-" + tc.Name, "route": "normal", "model": "gpt-test", "effort": "medium",
				"phase": "implementation", "scope": []any{"src"}}
			if tc.Name == "duplicate" {
				pending["job_id"] = jobID
			}
			pendingFile := filepath.Join(root, "pending", owner+".json")
			if err := os.MkdirAll(filepath.Dir(pendingFile), 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeFixtureJSON(pendingFile, pending); err != nil {
				t.Fatal(err)
			}
			admission := lease.New(filepath.Join(root, "projects", project)).Acquire(owner, pending["task_id"].(string),
				[]string{"src"}, lease.AcquireOptions{JobID: &jobID}, 1800000000)
			if admission.Status != "OK" {
				t.Fatalf("lease setup: %+v", admission)
			}
			response := map[string]any{"jobId": jobID, "state": "RUNNING"}
			switch tc.Name {
			case "failed":
				response = map[string]any{"jobId": jobID, "state": "FAILED", "errorCode": "START_FAILED"}
			case "malformed":
				response = map[string]any{"state": "RUNNING"}
			case "another_project":
				response = map[string]any{"jobId": strings.Repeat("a", 64), "state": "RUNNING"}
			case "duplicate":
				index := map[string]any{"jobId": jobID, "call": call, "call_key": owner, "session_id": event["session_id"], "project_key": project}
				if err := writeFixtureJSON(filepath.Join(root, "async_jobs", jobID+".json"), index); err != nil {
					t.Fatal(err)
				}
			}
			event["tool_response"] = map[string]any{"structuredContent": response}
			got, err := postStart(event, root, "__PYTHON__", "__WAITER__")
			if err != nil {
				t.Fatal(err)
			}
			normalizedOutput := normalizePostStart(got, root, repo, project)
			if !reflect.DeepEqual(normalizedOutput, tc.Output) {
				gotJSON, _ := json.Marshal(normalizedOutput)
				wantJSON, _ := json.Marshal(tc.Output)
				t.Errorf("output mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
			}
			var gotIndex any
			if indexData, readErr := os.ReadFile(filepath.Join(root, "async_jobs", jobID+".json")); readErr == nil {
				var index map[string]any
				_ = json.Unmarshal(indexData, &index)
				gotIndex = index
			}
			gotIndex = normalizePostStart(gotIndex, root, repo, project)
			if tc.Index == nil && gotIndex != nil || tc.Index != nil && !reflect.DeepEqual(gotIndex, tc.Index) {
				t.Errorf("index = %#v, want %#v", gotIndex, tc.Index)
			}
			var gotPending any
			if pendingData, readErr := os.ReadFile(pendingFile); readErr == nil {
				var pending map[string]any
				_ = json.Unmarshal(pendingData, &pending)
				gotPending = pending
			}
			gotPending = normalizePostStart(gotPending, root, repo, project)
			if tc.Pending == nil && gotPending != nil || tc.Pending != nil && !reflect.DeepEqual(gotPending, tc.Pending) {
				t.Errorf("pending = %#v, want %#v", gotPending, tc.Pending)
			}
			leaseData, readErr := os.ReadFile(filepath.Join(root, "projects", project, "scope_leases.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			var leaseRecord map[string]any
			if err := json.Unmarshal(leaseData, &leaseRecord); err != nil {
				t.Fatal(err)
			}
			states := []string{}
			for _, raw := range mapValue(leaseRecord["owners"]) {
				states = append(states, stringValue(mapValue(raw)["state"]))
			}
			if !reflect.DeepEqual(states, tc.LeaseStates) {
				t.Errorf("lease states = %v, want %v", states, tc.LeaseStates)
			}
		})
	}
}

func jobHash(call string) string {
	sum := sha256.Sum256([]byte(call))
	return hex.EncodeToString(sum[:])
}

func writeFixtureJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func normalizePostStart(value any, root, repo, project string) any {
	switch item := value.(type) {
	case string:
		item = strings.ReplaceAll(item, root, "__ROOT__")
		item = strings.ReplaceAll(item, repo, "__ROOT__/repo")
		item = strings.ReplaceAll(item, project, "__PROJECT_KEY__")
		return item
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			out[key] = normalizePostStart(child, root, repo, project)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			out[i] = normalizePostStart(child, root, repo, project)
		}
		return out
	default:
		return value
	}
}
