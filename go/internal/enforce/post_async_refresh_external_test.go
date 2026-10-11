package enforce_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"purser-hook/internal/enforce"
	"purser-hook/internal/jobs"
	"purser-hook/internal/ledger"
)

func TestHostValidationRedBeforeEditThenGreenFinal(t *testing.T) {
	stream := jobs.NewEventStream("codex")
	stream.Write([]byte(`{"type":"item.completed","item":{"type":"command_execution","command":"pytest tests/test_fix.py -k regression","exit_code":1}}
{"type":"item.completed","item":{"type":"file_change","status":"completed","changes":[{"path":"src/fix.py","kind":"update"}]}}
{"type":"item.completed","item":{"type":"command_execution","command":"pytest tests/","exit_code":0}}
`))
	evidence := jobs.TestEvidence(stream.Events())
	if len(evidence) != 1 || evidence[0]["exit_code"] != 0 || jobs.TestEvidenceFailed(stream.Events()) {
		t.Fatalf("pre-fix failure poisoned final validation: %#v", evidence)
	}
}

func TestHostValidationFinalFailureAfterLastEdit(t *testing.T) {
	stream := jobs.NewEventStream("codex")
	stream.Write([]byte(`{"type":"item.completed","item":{"type":"file_change","status":"completed","changes":[{"path":"src/fix.js","kind":"update"}]}}
{"type":"item.completed","item":{"type":"command_execution","command":"npm test -- regression","exit_code":1}}
{"type":"item.completed","item":{"type":"command_execution","command":"npm test -- other","exit_code":0}}
`))
	evidence := jobs.TestEvidence(stream.Events())
	if len(evidence) != 2 || evidence[0]["exit_code"] != 1 || !jobs.TestEvidenceFailed(stream.Events()) {
		t.Fatalf("final failure was hidden by later unrelated pass: %#v", evidence)
	}
}

func TestAsyncReviewRefreshAfterStaleApply(t *testing.T) {
	root := t.TempDir()
	repo, worktree := filepath.Join(root, "repo"), filepath.Join(root, "worktree")
	for _, dir := range []string{repo, filepath.Join(repo, "src")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	gitRefresh(t, repo, "init", "-q")
	writeRefresh(t, filepath.Join(repo, "src", "file.go"), "package src\n\nfunc A() {}\n\nfunc B() {}\n")
	gitRefresh(t, repo, "add", "--all")
	gitRefresh(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "base")
	gitRefresh(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	baseSnapshot, err := ledger.CheckoutSnapshot(worktree)
	if err != nil {
		t.Fatal(err)
	}
	writeRefresh(t, filepath.Join(worktree, "src", "file.go"), "package src\n\nfunc A() { /* worker change */ }\n\nfunc B() {}\n")

	call, session, origin, thread := "review refresh regression", "refresh-session", "refresh-origin", "refresh-thread"
	id := refreshHash(call)
	keyHash := sha256.Sum256([]byte(session + "\x00" + origin))
	callKey := hex.EncodeToString(keyHash[:])
	project := "refresh-project"
	baseline := ledger.CaptureBaseline(repo, []string{"src"}, 1)
	pending := map[string]any{"task_id": "refresh-task", "phase": "implementation", "scope": []any{"src"}, "baseline": baseline,
		"project_key": project, "repo_root": repo, "session_id": session, "tool_use_id": origin, "job_id": id, "async_call": call,
		"route": "normal", "model": "openai/gpt-5", "effort": "medium", "task_contract_grounded": true,
		"protected_paths": []any{}, "force_full_review": true, "skip_max_lines": 0, "skip_max_files": 0,
		"configured": true, "user_status": "ok", "project_status": "ok", "reviewer_candidates": []any{}, "reviewer": "none"}
	job := map[string]any{"jobId": id, "call": call, "state": "SUCCEEDED", "threadId": thread, "repoRoot": repo,
		"worktreePath": worktree, "snapshot": baseSnapshot, "reviewedSnapshot": baseSnapshot, "snapshotRevision": float64(1)}
	index := map[string]any{"jobId": id, "call": call, "call_key": callKey, "project_key": project, "session_id": session, "stage": "READY"}
	writeRefreshJSON(t, filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"), job)
	writeRefreshJSON(t, filepath.Join(root, "async_jobs", id+".json"), index)
	writeRefreshJSON(t, filepath.Join(root, "pending", callKey+".json"), pending)

	resultEvent := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_result", "session_id": session,
		"tool_use_id": "refresh-result", "cwd": repo, "tool_input": map[string]any{"jobId": id},
		"tool_response": map[string]any{"structuredContent": map[string]any{"jobId": id, "state": "SUCCEEDED", "threadId": thread, "content": "done"}}}
	callResult := func() {
		t.Helper()
		if _, err := enforce.PostToolUse(resultEvent, root); err != nil {
			t.Fatal(err)
		}
	}
	callResult()
	applyArgs := func(accept bool, revision string) map[string]any {
		return map[string]any{"jobId": id, "acceptOnly": accept, "sessionId": session, "revision": revision}
	}
	digest, err := ledger.JobChangedPathsSHA256(worktree, baseSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	accepted := jobs.Apply(root, applyArgs(true, digest))["structuredContent"].(map[string]any)
	if accepted["accepted"] != true {
		t.Fatalf("initial acceptOnly: %#v", accepted)
	}
	writeRefresh(t, filepath.Join(repo, "src", "file.go"), "package src\n\nfunc A() {}\n\nfunc B() { /* checkout drift */ }\n")
	gitRefresh(t, repo, "add", "src/file.go")
	gitRefresh(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "checkout drift")
	refused := jobs.Apply(root, map[string]any{"jobId": id})["structuredContent"].(map[string]any)
	if refused["errorCode"] != "REVIEW_STALE_REPREPARE_REQUIRED" {
		t.Fatalf("drift apply: %#v", refused)
	}

	callResult()
	jobAfter, err := os.ReadFile(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var refreshed map[string]any
	if err = json.Unmarshal(jobAfter, &refreshed); err != nil {
		t.Fatal(err)
	}
	newSnapshot := refreshed["snapshot"].(string)
	newDigest, err := ledger.JobChangedPathsSHA256(worktree, newSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	accepted = jobs.Apply(root, applyArgs(true, newDigest))["structuredContent"].(map[string]any)
	if accepted["accepted"] != true {
		indexAfter, _ := os.ReadFile(filepath.Join(root, "async_jobs", id+".json"))
		diagnostic, _ := os.ReadFile(filepath.Join(root, "diagnostics.jsonl"))
		t.Fatalf("refreshed acceptOnly: %#v job=%#v index=%s diagnostic=%s", accepted, refreshed, indexAfter, diagnostic)
	}
	applied := jobs.Apply(root, map[string]any{"jobId": id})["structuredContent"].(map[string]any)
	if applied["applied"] != true {
		t.Fatalf("apply after refresh: %#v", applied)
	}
}

func refreshHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func gitRefresh(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
func writeRefresh(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func writeRefreshJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
