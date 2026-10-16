package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/enforce"
	"purser-hook/internal/ledger"
	"purser-hook/internal/testutil"
	"purser-hook/internal/waiter"
)

func TestMCPPreparationBindingAndPolicyParity(t *testing.T) {
	parent := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, "mcp-preparation-")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.Abs(root)
	defer os.RemoveAll(root)
	owner, err := waiter.PinOwner(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "stale-env-session")
	e := map[string]any{"hook_event_name": "PreToolUse", "session_id": "current-hook-session", "tool_use_id": "exact-join",
		"tool_name": "mcp__plugin_purser_codex-main__codex_apply", "cwd": root, "agent_id": "agent",
		"tool_input": map[string]any{"jobId": "job", "acceptOnly": true, "sessionId": "forged"}}
	if err := enforce.PublishMCPHandoff(root, owner, e); err != nil {
		t.Fatal(err)
	}
	for _, wire := range []string{
		`{"name":"codex_apply","arguments":{},"_meta":{}}`,
		`{"name":"codex_apply","arguments":{},"_meta":{"claudecode/toolUseId":"exact-join","claudecode/toolUseId":"other"}}`,
		`{"name":"codex_apply","arguments":{},"_meta":{"claudecode/toolUseId":"exact-join"}}`,
	} {
		if _, err := prepareMCPCall(root, owner, []byte(wire)); err == nil {
			t.Fatal("bad metadata/binding accepted")
		}
	}
	params, _ := json.Marshal(map[string]any{"name": "codex_apply", "arguments": e["tool_input"], "_meta": map[string]any{"claudecode/toolUseId": "exact-join"}})
	want, _ := enforce.Pre(e, root)
	h, err := prepareMCPCall(root, owner, params)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.Response, want) || h.SessionID != "current-hook-session" || !reflect.DeepEqual(h.Effective, e["tool_input"]) {
		t.Fatalf("policy/identity mismatch: %+v", h)
	}
	read, err := enforce.ReadPreparedMCPHandoff(root, owner, h.SessionID, h.ToolUseID)
	if err != nil || !reflect.DeepEqual(read.Response, want) {
		t.Fatalf("post recovery: %v", err)
	}
	if _, err := enforce.ReadPreparedMCPHandoff(root, owner, "forged", h.ToolUseID); err == nil {
		t.Fatal("wrong session recovered")
	}
	if _, err := prepareMCPCall(root, owner, params); err == nil {
		t.Fatal("replayed preparation")
	}
}

func TestMCPPreparedApplyAcceptanceGate(t *testing.T) {
	root, owner, jobID, revision, jobPath := mcpApplyFixture(t)
	input := func(acceptOnly bool) map[string]any {
		value := map[string]any{"jobId": jobID}
		if acceptOnly {
			value["acceptOnly"] = true
			value["revision"] = revision
		}
		return value
	}
	publish := func(t *testing.T, id, agent string, value map[string]any) []byte {
		t.Helper()
		e := map[string]any{"hook_event_name": "PreToolUse", "session_id": "hook-session", "tool_use_id": id,
			"tool_name": "mcp__plugin_purser_codex-main__codex_apply", "cwd": root, "agent_id": agent, "tool_input": value}
		if err := enforce.PublishMCPHandoff(root, owner, e); err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(map[string]any{"name": "codex_apply", "arguments": value,
			"_meta": map[string]any{"claudecode/toolUseId": id}})
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	response := func(t *testing.T, id, agent string, value map[string]any) map[string]any {
		t.Helper()
		h, err := prepareMCPCall(root, owner, publish(t, id, agent, value))
		if err != nil {
			t.Fatal(err)
		}
		return h.Response
	}
	isDenied := func(value map[string]any) bool {
		output, _ := value["hookSpecificOutput"].(map[string]any)
		return output["permissionDecision"] == "deny"
	}
	if denied := response(t, "without-acceptance", "subagent", input(false)); !isDenied(denied) {
		t.Fatalf("subagent apply without acceptance passed: %#v", denied)
	}
	if allowed := response(t, "main-apply", "", input(false)); isDenied(allowed) {
		t.Fatalf("main-thread apply was denied: %#v", allowed)
	}
	accepted := response(t, "main-acceptance", "", input(true))
	acceptedOutput, _ := accepted["hookSpecificOutput"].(map[string]any)
	if acceptedOutput["updatedInput"] == nil {
		t.Fatalf("main acceptance did not pass: %#v", accepted)
	}
	var acceptedJob map[string]any
	jobData, err := os.ReadFile(jobPath)
	if err != nil || json.Unmarshal(jobData, &acceptedJob) != nil {
		t.Fatalf("read acceptance fixture: %v", err)
	}
	acceptedJob["acceptance"] = map[string]any{"revision": revision, "sessionId": "hook-session", "acceptedAt": "now"}
	jobData, _ = json.Marshal(acceptedJob)
	if err := os.WriteFile(jobPath, jobData, 0600); err != nil {
		t.Fatal(err)
	}
	if allowed := response(t, "after-acceptance", "subagent", input(false)); isDenied(allowed) {
		t.Fatalf("subagent apply after main acceptance was denied: %#v", allowed)
	}
	jobData, err = os.ReadFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	var broken map[string]any
	if err := json.Unmarshal(jobData, &broken); err != nil {
		t.Fatal(err)
	}
	delete(broken, "reviewedSnapshot")
	brokenData, _ := json.Marshal(broken)
	if err := os.WriteFile(jobPath, brokenData, 0600); err != nil {
		t.Fatal(err)
	}
	failure := response(t, "helper-failure", "", input(true))
	failureOutput, _ := failure["hookSpecificOutput"].(map[string]any)
	if failureOutput["permissionDecision"] != "deny" {
		t.Fatalf("acceptOnly helper failure passed: %#v", failure)
	}
}
func mcpApplyFixture(t *testing.T) (root string, owner waiter.Owner, jobID, revision, jobPath string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "state")
	repo, worktree := filepath.Join(base, "repo"), filepath.Join(base, "worktree")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitMCPTest(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitMCPTest(t, repo, "add", "--all")
	gitMCPTest(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	gitMCPTest(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	snapshot := strings.TrimSpace(string(gitMCPOutput(t, worktree, "rev-parse", "HEAD^{tree}")))
	if err := os.WriteFile(filepath.Join(worktree, "tracked.txt"), []byte("reviewed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	jobID = strings.Repeat("e", 64)
	jobPath = filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
	if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"jobId": jobID, "repoRoot": repo, "worktreePath": worktree, "snapshot": snapshot, "reviewedSnapshot": snapshot}
	write := func() {
		data, _ := json.Marshal(job)
		if err := os.WriteFile(jobPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	var err error
	revision, err = ledger.JobChangedPathsSHA256(worktree, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	owner, err = waiter.PinOwner(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return
}

func gitMCPTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testutil.Hide(exec.Command("git", append([]string{"-C", dir}, args...)...))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func gitMCPOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := testutil.Hide(exec.Command("git", append([]string{"-C", dir}, args...)...))
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}
