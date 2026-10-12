package enforce

import "purser-hook/internal/testutil"

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/ledger"
	"strings"
	"testing"
)

func TestPreApplyAcceptanceGate(t *testing.T) {
	base := t.TempDir()
	repo, worktree, state := filepath.Join(base, "repo"), filepath.Join(base, "worktree"), filepath.Join(base, "state")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "--all")
	git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	snapshot := strings.TrimSpace(string(gitOutput(t, worktree, "rev-parse", "HEAD^{tree}")))
	if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), []byte("reviewed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 64)
	jobPath := filepath.Join(state, "codex_exec_mcp", "jobs", id+".json")
	if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"jobId": id, "state": "SUCCEEDED", "repoRoot": repo, "worktreePath": worktree,
		"snapshot": snapshot, "reviewedSnapshot": snapshot}
	writeJSON := func() {
		raw, _ := json.Marshal(job)
		if err := os.WriteFile(jobPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON()
	tool := "mcp__codex-main__codex_apply"
	event := func(input map[string]any, agent any) map[string]any {
		value := map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input, "session_id": "session-main"}
		if agent != nil {
			value["agent_id"] = agent
		}
		return value
	}
	decision := func(value map[string]any) map[string]any {
		got, err := Pre(value, state)
		if err != nil {
			t.Fatal(err)
		}
		return mapValue(got["hookSpecificOutput"])
	}
	input := map[string]any{"jobId": id}
	if denied := decision(event(input, "agent-1")); denied["permissionDecisionReason"] != "main Claude has not accepted this revision" {
		t.Fatalf("subagent without acceptance was not denied: %#v", denied)
	}
	if denied := decision(event(map[string]any{"jobId": id, "acceptOnly": true}, "agent-1")); denied["permissionDecision"] != "deny" {
		t.Fatalf("subagent acceptOnly was not denied: %#v", denied)
	}
	reviewedRevision, err := ledger.JobChangedPathsSHA256(worktree, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	main := decision(event(map[string]any{"jobId": id, "acceptOnly": true, "revision": reviewedRevision}, nil))
	updated := mapValue(main["updatedInput"])
	revision, ok := updated["revision"].(string)
	if !ok || revision != reviewedRevision || updated["sessionId"] != "session-main" {
		t.Fatalf("main acceptance was not bound to hook revision and session: %#v", main)
	}
	job["acceptance"] = map[string]any{"revision": revision, "sessionId": "session-main", "acceptedAt": "now"}
	writeJSON()
	if allowed := decision(event(input, "agent-1")); allowed["permissionDecision"] == "deny" {
		t.Fatalf("matching acceptance did not allow subagent apply: %#v", allowed)
	}
	if err := os.WriteFile(filepath.Join(worktree, "new.txt"), []byte("drift\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if denied := decision(event(input, "agent-1")); denied["permissionDecisionReason"] != "main Claude has not accepted this revision" {
		t.Fatalf("old revision acceptance survived drift: %#v", denied)
	}
	if allowed := decision(event(input, nil)); allowed["permissionDecision"] == "deny" {
		t.Fatalf("foreground apply without acceptance was gated: %#v", allowed)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := testutil.Hide(exec.Command("git", append([]string{"-C", dir}, args...)...))
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}
