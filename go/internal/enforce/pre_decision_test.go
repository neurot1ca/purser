package enforce

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/ledger"
)

func TestPreDecisionsP63Fixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/pre_decisions.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string         `json:"name"`
			Tool     string         `json:"tool"`
			Input    map[string]any `json:"input"`
			State    string         `json:"state"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&fixture); err != nil || fixture.Oracle != "Python codex.pre_tool_use" {
		t.Fatalf("fixture decode: %v %q", err, fixture.Oracle)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			if item.Expected == nil {
				t.Fatal("Python oracle fixture has no expected decision")
			}
			scratch := preTempDir(t)
			repo := filepath.Join(scratch, "repo")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Claude runs the full suite once before acceptance.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			home, runtimeRoot := filepath.Join(scratch, "home"), filepath.Join(scratch, "state")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOMEDRIVE", "")
			t.Setenv("HOMEPATH", "")
			pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PURSER_STATE_DIR", runtimeRoot)
			t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			input := replaceRepo(item.Input, repo)
			if item.State == "thread" {
				seedPreThread(t, repo, runtimeRoot)
			}
			if item.State == "fallback" {
				if err := os.MkdirAll(filepath.Join(home, ".orchestrator"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".orchestrator", "config.toml"), []byte("[fallback]\nagent = \"agy\"\nmodel = \"gemini-3.8-flash-high\"\nfamily = \"google\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				seedFallback(t, repo, runtimeRoot)
			}
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": item.Tool, "tool_input": input,
				"session_id": "session-p63", "tool_use_id": "use-p63"}
			got, gotErr := Pre(event, runtimeRoot)
			if item.Tool == "mcp__codex-main__codex_start" {
				output := mapValue(got["hookSpecificOutput"])
				if gotErr != nil || mapValue(output["updatedInput"]) == nil || output["permissionDecision"] != nil {
					t.Fatalf("async start was not durably admitted: %#v %v", got, gotErr)
				}
				pending, _ := pendingPath(runtimeRoot, event)
				if raw, readErr := os.ReadFile(pending); readErr != nil || !strings.Contains(string(raw), `"async_ready":true`) {
					t.Fatalf("async start pending record is not ready: %v %s", readErr, raw)
				}
				return
			}
			want := replaceFixtureProjectKey(item.Expected, projectKey(&repo))
			gotNormalized := replaceTemporaryRoot(got, scratch)
			gotJSON, _ := canonicalJSON(gotNormalized)
			wantJSON, _ := canonicalJSON(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("Pre decision mismatch\n got: %s\nwant: %s\nerr: %v", gotJSON, wantJSON, gotErr)
			}
		})
	}
}

func TestPreTelemetryScopePathCount(t *testing.T) {
	scratch := preTempDir(t)
	repo, runtimeRoot := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	home, _ := filepath.Abs(filepath.Join(scratch, "home"))
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PURSER_STATE_DIR", runtimeRoot)
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
	const prompt = "ORCH|task=scope-count|phase=implementation|route=normal|reason=new_task|basis=specific boundary|scope=src/a.go,src/b.go\nImplement."
	event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__codex-main__codex", "tool_use_id": "scope-count-call",
		"session_id": "scope-count-session", "cwd": repo, "tool_input": map[string]any{"prompt": prompt, "label": "scope count"}}
	if _, err := Pre(event, runtimeRoot); err != nil {
		t.Fatalf("Pre: %v", err)
	}
	path := filepath.Join(runtimeRoot, "telemetry", "events", "by_task", "scope-count.jsonl")
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(file), []byte{'\n'}) {
		var observation map[string]any
		if err := json.Unmarshal(line, &observation); err != nil {
			t.Fatal(err)
		}
		if observation["event_type"] == "TASK_ROUTE_SELECTED" {
			payload := mapValue(observation["payload"])
			if payload["scope_path_count"] != float64(2) || payload["scope"] != nil {
				t.Fatalf("unexpected scope telemetry: %#v", payload)
			}
			return
		}
	}
	t.Fatal("TASK_ROUTE_SELECTED observation missing")
}

func replaceFixtureProjectKey(value any, project string) any {
	switch value := value.(type) {
	case string:
		return strings.ReplaceAll(value, "__PROJECT_KEY__", project)
	case map[string]any:
		for key, item := range value {
			value[key] = replaceFixtureProjectKey(item, project)
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = replaceFixtureProjectKey(item, project)
		}
		return value
	default:
		return value
	}
}

func replaceRepo(input map[string]any, repo string) map[string]any {
	copy := make(map[string]any, len(input))
	for key, value := range input {
		if value == "__REPO__" {
			value = repo
		}
		copy[key] = value
	}
	return copy
}
func replaceTemporaryRoot(value any, scratch string) any {
	switch value := value.(type) {
	case string:
		return strings.ReplaceAll(value, scratch, "__TMP_ROOT__")
	case map[string]any:
		for key, item := range value {
			value[key] = replaceTemporaryRoot(item, scratch)
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = replaceTemporaryRoot(item, scratch)
		}
		return value
	default:
		return value
	}
}
func seedPreThread(t *testing.T, repo, runtimeRoot string) {
	t.Helper()
	project := filepath.Join(runtimeRoot, "projects", projectKey(&repo))
	thread := "thread-p63"
	record := ledger.BuildSessionRecord("task-recorded", &thread, "subtle", "gpt-6-luna", "high", map[string]any{
		"phase": "implementation", "scope": []any{"src/a.go"}, "label": "saved label", "task_contract_grounded": false,
	})
	if result := ledger.Save(project, record); result.Status != ledger.OK {
		t.Fatalf("seed ledger: %#v", result)
	}
	sum := sha256.Sum256([]byte(thread))
	path := filepath.Join(runtimeRoot, "threads", hex.EncodeToString(sum[:])+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]any{"thread_id": thread, "project_root": repo})
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}
func seedFallback(t *testing.T, repo, runtimeRoot string) {
	t.Helper()
	project := filepath.Join(runtimeRoot, "projects", projectKey(&repo))
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"active_coder": "fallback", "triggering_evidence": map[string]any{"type": "RATE_LIMITED", "trigger_id": "rate-1", "switched_at": "99999999999", "retryAfterSeconds": 3600}, "reset_cycle_marker": nil, "last_probe_time": nil}
	encoded, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(project, "active-coder.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}
