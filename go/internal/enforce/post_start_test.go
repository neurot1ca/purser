package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestPostStartFallbackWaiterPathIsAbsolute(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".claude", "hooks", "orchestrator"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", repo)
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_start",
		"tool_use_id": "absolute-waiter", "session_id": "waiter-session"}
	call, _ := callKey(event)
	jobID := jobHash("waiter-call")
	pending := map[string]any{"async_call": "waiter-call", "async_ready": true, "session_id": "waiter-session",
		"tool_use_id": "absolute-waiter", "project_key": "project"}
	path := filepath.Join(root, "pending", call+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeFixtureJSON(path, pending); err != nil {
		t.Fatal(err)
	}
	event["tool_response"] = map[string]any{"structuredContent": map[string]any{"jobId": jobID, "state": "RUNNING"}}
	got, err := postStart(event, root, "python", "")
	if err != nil {
		t.Fatal(err)
	}
	output := mapValue(got["hookSpecificOutput"])
	entries, _ := output["updatedToolOutput"].([]any)
	if len(entries) == 0 {
		t.Fatalf("post-start response has no output: %#v", got)
	}
	text := stringValue(mapValue(entries[0])["text"])
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	waiter := filepath.Join(repo, ".claude", "hooks", "orchestrator", "purser_wait.py")
	if !filepath.IsAbs(waiter) || !strings.Contains(stringValue(payload["next"]), `"`+waiter+`"`) {
		t.Fatalf("waiter path is not absolute in response: %s", text)
	}
}

func TestPostStartWaiterUsesExecutableRoot(t *testing.T) {
	const childEnv = "PURSER_WAITER_PATH_CHILD"
	if plugin := os.Getenv(childEnv); plugin != "" {
		if os.Getenv("CLAUDE_PLUGIN_ROOT") != "" {
			t.Fatal("CLAUDE_PLUGIN_ROOT must be unset")
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(plugin, ".claude", "hooks", "orchestrator", "purser_wait.py")
		if strings.HasPrefix(filepath.Clean(want)+string(os.PathSeparator), filepath.Clean(cwd)+string(os.PathSeparator)) {
			t.Fatalf("fixture waiter unexpectedly under cwd: %s", want)
		}
		root := t.TempDir()
		event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_start",
			"tool_use_id": "executable-root-waiter", "session_id": "waiter-session"}
		key, err := callKey(event)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeFixtureJSON(filepath.Join(root, "pending", key+".json"), map[string]any{
			"async_call": "executable-root-call", "async_ready": true, "project_key": "project",
		}); err != nil {
			t.Fatal(err)
		}
		event["tool_response"] = map[string]any{"structuredContent": map[string]any{
			"jobId": jobHash("executable-root-call"), "state": "RUNNING",
		}}
		got, err := postStart(event, root, "python", "")
		if err != nil {
			t.Fatal(err)
		}
		output := mapValue(got["hookSpecificOutput"])
		entries, _ := output["updatedToolOutput"].([]any)
		if len(entries) == 0 {
			t.Fatalf("waiter command output is missing: %#v", got)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(stringValue(mapValue(entries[0])["text"])), &payload); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stringValue(payload["next"]), want) {
			t.Fatalf("waiter command did not use executable-root path %s: %#v", want, got)
		}
		return
	}

	temp := t.TempDir()
	plugin := filepath.Join(temp, "plugin")
	bin := filepath.Join(plugin, "bin")
	waiter := filepath.Join(plugin, ".claude", "hooks", "orchestrator", "purser_wait.py")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(waiter), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(waiter, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(bin, "purser-hook-test")
	if runtime.GOOS == "windows" {
		child += ".exe"
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, data, 0700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(temp, "unrelated")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(child, "-test.run=^TestPostStartWaiterUsesExecutableRoot$")
	command.Dir = cwd
	command.Env = []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CLAUDE_PLUGIN_ROOT=") && !strings.HasPrefix(entry, childEnv+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, childEnv+"="+plugin)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child waiter path check: %v\n%s", err, output)
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
