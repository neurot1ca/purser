package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"purser-hook/internal/enforce"
	"purser-hook/internal/jobs"
)

func TestSynchronousCursorNewAndReply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows supervisor teardown is covered by existing supervision tests; no new real-process acceptance")
	}
	bin := t.TempDir()
	writeFakeCursor(t, bin)
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+oldPath)
	t.Setenv("PURSER_STATE_DIR", t.TempDir())
	t.Setenv("CURSOR_TEST_SESSION", "cursor-thread")
	cwd := t.TempDir()

	for _, name := range []string{"codex", "codex-reply"} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"agent": "cursor", "prompt": "implement this", "cwd": cwd, "model": "claude-4.5-sonnet"}
			if name == "codex-reply" {
				args["threadId"] = "cursor-thread"
				if err := jobs.SaveRecord(jobs.StateDir(), map[string]any{"thread_id": "cursor-thread", "cwd": cwd, "model": "claude-4.5-sonnet", "config_args": []any{}, "agent": "cursor"}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := runSynchronousContext(context.Background(), args, name)
			if err != nil {
				t.Fatal(err)
			}
			if result["isError"] == true {
				t.Fatalf("Cursor %s failed: %#v", name, result)
			}
			structured, _ := result["structuredContent"].(map[string]any)
			if structured["threadId"] != "cursor-thread" || structured["content"] != "finished" {
				t.Fatalf("Cursor %s response = %#v", name, structured)
			}
			record := jobs.LoadRecord(jobs.StateDir(), "cursor-thread")
			if record == nil || record["agent"] != "cursor" {
				t.Fatalf("Cursor attribution was not persisted: %#v", record)
			}
		})
	}
}

// Mirrors TestCodexStartOpenCodeIgnoresEmptyCodexOverride at the injected launch seam.
func TestAsyncCursorNewAndReply(t *testing.T) {
	bin, state, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	writeFakeCursor(t, bin) // Resolved during validation, never executed.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for i, mode := range []string{"new", "reply"} {
		t.Run(mode, func(t *testing.T) {
			args := map[string]any{"agent": "cursor", "call": "123e4567-e89b-12d3-a456-42661417400" + string(rune('1'+i)),
				"mode": mode, "prompt": "implement this", "cwd": cwd, "model": "claude-4.5-sonnet"}
			name := "codex"
			if mode == "reply" {
				name = "codex-reply"
				args["threadId"] = "cursor-thread"
				if err := jobs.SaveRecord(filepath.Join(state, "codex_exec_mcp", "threads"), map[string]any{
					"thread_id": "cursor-thread", "agent": "cursor", "model": "claude-4.5-sonnet", "cwd": cwd, "repoRoot": cwd, "config_args": []any{},
				}); err != nil {
					t.Fatal(err)
				}
			}
			launches := 0
			result, _, err := jobs.Start(state, args, jobs.StartOptions{
				CodexBin: "must-not-leak",
				Validation: jobs.StartValidationOptions{ValidateAgent: func(agent string) error {
					if agent != "cursor" {
						t.Fatalf("wrong agent lookup: %s", agent)
					}
					_, err := jobs.ResolveAgentPrefix(agent, nil)
					return err
				}},
				Launch: func(id string, _ int64, _ string) error {
					launches++
					data, err := os.ReadFile(filepath.Join(state, "codex_exec_mcp", "jobs", id+".launch"))
					if err != nil {
						return err
					}
					var launch map[string]any
					if err := json.Unmarshal(data, &launch); err != nil {
						return err
					}
					input := launch["args"].(map[string]any)
					if launch["agent"] != "cursor" || launch["name"] != name || launch["codexBin"] != nil || input["prompt"] != "implement this" || input["threadId"] != args["threadId"] {
						t.Fatalf("wrong async wiring: %s", data)
					}
					return nil
				},
			})
			if err != nil || result["isError"] == true || launches != 1 {
				t.Fatalf("async %s: launches=%d result=%#v err=%v", mode, launches, result, err)
			}
		})
	}
}

func TestUnselectedCursorLeavesCodexJobUnchanged(t *testing.T) {
	plugin, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// Keep the test name out of generated CODE_MAP paths in the worker prompt.
	scratch, err := os.MkdirTemp(filepath.Join(plugin, ".tmp"), "codex-only-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	repo, state, home := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state"), filepath.Join(scratch, "home")
	for _, dir := range []string{repo, state, home} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("PURSER_AGENT", "")
	gitMCPTest(t, repo, "init", "-q")
	gitMCPTest(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "baseline")
	event := map[string]any{"hook_event_name": "PreToolUse", "session_id": "codex-only", "tool_use_id": "codex-only",
		"tool_name": "mcp__codex-main__codex", "cwd": repo, "tool_input": map[string]any{"cwd": repo,
			"prompt": "ORCH|task=codex-only|phase=implementation|route=subtle|scope=src\n## Objective\nInspect src.\n## Acceptance criteria\n- Report result."}}
	pre, err := enforce.Pre(event, state)
	if err != nil {
		t.Fatal(err)
	}
	input, ok := pre["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)
	if !ok || strings.Contains(strings.ToLower(input["prompt"].(string)), "cursor") {
		t.Fatalf("Codex prompt changed: %#v", pre)
	}
	input["call"] = "123e4567-e89b-12d3-a456-426614174009"
	lookups, launches := 0, 0
	result, _, err := jobs.Start(state, input, jobs.StartOptions{
		Validation: jobs.StartValidationOptions{ValidateAgent: func(agent string) error {
			lookups++
			if agent != "codex" {
				t.Fatalf("unselected agent lookup: %s", agent)
			}
			return nil
		}},
		Launch: func(id string, _ int64, _ string) error {
			launches++
			data, err := os.ReadFile(filepath.Join(state, "codex_exec_mcp", "jobs", id+".launch"))
			if err != nil {
				return err
			}
			var launch map[string]any
			if err := json.Unmarshal(data, &launch); err != nil {
				return err
			}
			if launch["agent"] != "codex" || strings.Contains(strings.ToLower(string(data)), "cursor") {
				t.Fatalf("unselected worker launched: %s", data)
			}
			return nil
		},
	})
	if err != nil || result["isError"] == true || lookups != 1 || launches != 1 {
		t.Fatalf("Codex job: lookups=%d launches=%d result=%#v err=%v", lookups, launches, result, err)
	}
	event["hook_event_name"] = "PostToolUse"
	event["tool_response"] = map[string]any{"structuredContent": map[string]any{"threadId": "codex-thread", "content": "done"}}
	post, failure := enforce.PostToolUse(event, state)
	if failure != nil {
		t.Fatal(failure)
	}
	output := post["hookSpecificOutput"].(map[string]any)["updatedToolOutput"].([]any)[0].(map[string]any)
	var bundle map[string]any
	if err := json.Unmarshal([]byte(output["text"].(string)), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle["implementer"] != "codex-main" {
		t.Fatalf("Codex attribution lost: %#v", bundle)
	}
	for key := range bundle {
		if strings.Contains(strings.ToLower(key), "cursor") {
			t.Fatalf("Cursor-specific bundle field: %s", key)
		}
	}
}

func writeFakeCursor(t *testing.T, bin string) {
	t.Helper()
	cli := filepath.Join(bin, "cursor-agent")
	if runtime.GOOS == "windows" {
		cli += ".cmd"
		if err := os.WriteFile(cli, []byte("@echo off\r\necho {\"type\":\"system\",\"session_id\":\"%CURSOR_TEST_SESSION%\"}\r\necho {\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"%CURSOR_TEST_SESSION%\",\"result\":\"finished\"}\r\nexit /b 0\r\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"session_id\":\"'\"$CURSOR_TEST_SESSION\"'\"}' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"'\"$CURSOR_TEST_SESSION\"'\",\"result\":\"finished\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
}
