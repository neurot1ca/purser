package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/ledger"
)

func TestPreSuccessP64Fixture(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "pre_success.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string           `json:"oracle"`
		Cases  []map[string]any `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&fixture); err != nil || fixture.Oracle != "codex.pre_tool_use durable success path" {
		t.Fatalf("fixture decode: %v %q", err, fixture.Oracle)
	}
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		name := stringValue(tc["name"])
		t.Run(name, func(t *testing.T) {
			scratch := preTempDir(t)
			repo, runtimeRoot := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("p64 fixture baseline\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "-C", repo, "add", "README.md").CombinedOutput(); err != nil {
				t.Fatalf("git add: %v %s", err, out)
			}
			commit := exec.Command("git", "-C", repo, "-c", "user.name=Purser Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture baseline")
			commit.Env = append(os.Environ(), "GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000")
			if out, err := commit.CombinedOutput(); err != nil {
				t.Fatalf("git commit: %v %s", err, out)
			}
			home := filepath.Join(scratch, "home")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("PURSER_STATE_DIR", runtimeRoot)
			t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			input, _ := tc["input"].(map[string]any)
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": tc["tool"], "tool_use_id": name,
				"session_id": "p64-session", "cwd": repo, "tool_input": input}
			resolved, _ := resolveProjectRoot(event, nil)
			project := projectKey(resolved)
			if tc["lease_setup"] != nil {
				seedP64Lease(t, runtimeRoot, project, stringValue(tc["lease_setup"]))
			}
			if thread, ok := tc["thread_id"].(string); ok && thread != "" {
				seedP64Thread(t, repo, runtimeRoot, project, stringValue(tc["task_id"]), thread)
			}
			response := map[string]any{}
			if tc["failure"] == "session_save" {
				save := saveAdmissionSession
				saveAdmissionSession = func(string, map[string]any) ledger.Result {
					return ledger.Result{Status: "UNKNOWN", Code: "LEDGER_WRITE_FAILED"}
				}
				defer func() { saveAdmissionSession = save }()
			}
			got, preErr := Pre(event, runtimeRoot)
			if tc["failure"] == "session_save" && preErr == nil {
				t.Fatal("Pre unexpectedly succeeded after injected session save failure")
			}
			if tc["failure"] != "session_save" && preErr != nil {
				t.Fatalf("Pre: %v", preErr)
			}
			output := mapValue(got["hookSpecificOutput"])
			for _, field := range []string{"updatedInput", "permissionDecision", "permissionDecisionReason"} {
				if value, exists := output[field]; exists {
					response[field] = value
				}
			}
			if value, exists := got["systemMessage"]; exists {
				response["systemMessage"] = value
			}
			path, err := pendingPath(runtimeRoot, event)
			if err != nil {
				t.Fatal(err)
			}
			var pending any
			if raw, readErr := os.ReadFile(path); readErr == nil {
				if err = json.Unmarshal(raw, &pending); err != nil {
					t.Fatal(err)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			var session any
			sessionPath, _ := ledger.PathForTask(filepath.Join(runtimeRoot, "projects", project), stringValue(tc["task_id"]))
			if loaded := ledger.Load(sessionPath, stringValue(tc["task_id"])); loaded.Status == ledger.OK {
				session = loaded.Record
			}
			eventTypes := make([]any, 0)
			eventPath := filepath.Join(runtimeRoot, "telemetry", "events", "by_task", stringValue(tc["task_id"])+".jsonl")
			if raw, readErr := os.ReadFile(eventPath); readErr == nil {
				for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
					if len(line) == 0 {
						continue
					}
					var value map[string]any
					if json.Unmarshal(line, &value) != nil {
						t.Fatalf("bad event line: %s", line)
					}
					eventTypes = append(eventTypes, value["event_type"])
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			var leaseState any
			leasePath := filepath.Join(runtimeRoot, "projects", project, "scope_leases.json")
			if raw, readErr := os.ReadFile(leasePath); readErr == nil {
				if err = json.Unmarshal(raw, &leaseState); err != nil {
					t.Fatal(err)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			actual := map[string]any{"name": name, "tool": tc["tool"], "task_id": tc["task_id"], "input": input,
				"lease_setup": tc["lease_setup"], "thread_id": tc["thread_id"], "failure": tc["failure"],
				"response": response, "pending": pending, "session": session, "event_types": eventTypes, "lease": leaseState}
			if tc["failure"] == "session_save" {
				raw, readErr := os.ReadFile(filepath.Join(runtimeRoot, "diagnostics.jsonl"))
				if readErr != nil {
					t.Fatalf("read failure diagnostic: %v", readErr)
				}
				var diagnostic map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(raw), &diagnostic); err != nil {
					t.Fatalf("decode failure diagnostic: %v", err)
				}
				if diagnostic["tool_use_id"] != name || !strings.Contains(stringValue(diagnostic["error"]), "LEDGER_WRITE_FAILED") {
					t.Fatalf("failure diagnostic not recorded: %#v", diagnostic)
				}
			}
			actual = normalizeP64(actual, repo, runtimeRoot, project).(map[string]any)
			var differences []string
			for key, want := range tc {
				got, _ := json.Marshal(actual[key])
				expected, _ := json.Marshal(want)
				if !bytes.Equal(got, expected) {
					differences = append(differences, key+": got="+string(got)+" want="+string(expected))
				}
			}
			if len(differences) != 0 {
				t.Fatalf("p64 state mismatch:\n%s", strings.Join(differences, "\n"))
			}
		})
	}
}

func seedP64Lease(t *testing.T, root, project, setup string) {
	t.Helper()
	now := float64(time.Now().UnixNano()) / 1e9
	owner := map[string]any{"task_id": "p64-existing", "scope": []any{"src/a.go"}, "state": "ACTIVE", "job_id": "existing-job", "queued_at": now - 10, "acquired_at": now - 10, "retention_after": now + 3600}
	value := map[string]any{"version": 1, "owners": map[string]any{"existing-owner": owner}}
	if setup == "hot_conflict" {
		value["conflicts"] = []any{map[string]any{"path": "src/a.go", "recorded_at": now - 5}}
	}
	if err := atomicAdmissionJSON(filepath.Join(root, "projects", project, "scope_leases.json"), value); err != nil {
		t.Fatal(err)
	}
}

func seedP64Thread(t *testing.T, repo, root, project, task, thread string) {
	t.Helper()
	record := ledger.BuildSessionRecord(task, &thread, "operation", "gpt-6-luna", "high", map[string]any{
		"phase": "operation", "scope": []any{}, "label": "P64 prior", "task_contract_grounded": false,
	})
	if saved := ledger.Save(filepath.Join(root, "projects", project), record); saved.Status != ledger.OK {
		t.Fatalf("seed session: %#v", saved)
	}
	path, ok := threadIndexPath(root, thread)
	if !ok {
		t.Fatal("invalid thread id")
	}
	if err := atomicAdmissionJSON(path, map[string]any{"thread_id": thread, "project_root": repo}); err != nil {
		t.Fatal(err)
	}
}

func normalizeP64(value any, repo, state, project string) any {
	switch item := value.(type) {
	case string:
		item = strings.ReplaceAll(item, repo, "__REPO__")
		item = strings.ReplaceAll(item, state, "__STATE__")
		item = strings.ReplaceAll(item, project, "__PROJECT_KEY__")
		if executable, err := os.Executable(); err == nil {
			item = strings.ReplaceAll(item, `"`+executable+`" leases --release`, `"__EXECUTABLE__" leases --release`)
		}
		return item
	case []any:
		for i := range item {
			item[i] = normalizeP64(item[i], repo, state, project)
		}
		return item
	case map[string]any:
		for key, child := range item {
			if strings.HasSuffix(key, "_at") || strings.HasSuffix(key, "_time") || strings.HasSuffix(key, "_after") {
				if _, ok := child.(json.Number); ok {
					item[key] = "__TIME__"
					continue
				}
				if _, ok := child.(float64); ok {
					item[key] = "__TIME__"
					continue
				}
			}
			item[key] = normalizeP64(child, repo, state, project)
		}
		return item
	default:
		return value
	}
}
