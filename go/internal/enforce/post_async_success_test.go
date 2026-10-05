package enforce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func TestFinishAsyncSuccessCachesReviewedNoDelta(t *testing.T) {
	state := t.TempDir()
	repo := filepath.Join(state, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	runAsyncSuccessGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "src", "file.go"), []byte("package src\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAsyncSuccessGit(t, repo, "add", "--all")
	runAsyncSuccessGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
	baseline := ledger.CaptureBaseline(repo, []string{"src"}, 1)
	if baseline["status"] != ledger.OK {
		t.Fatalf("baseline capture: %#v", baseline)
	}
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &baseline); err != nil {
		t.Fatal(err)
	}
	if delta := ledger.ComputeDelta(repo, baseline, []string{"src"}); delta["status"] != ledger.OK {
		t.Fatalf("baseline delta: %#v baseline=%#v", delta, baseline)
	}
	project, owner, task := "project-fixture", "lease-fixture", "task-fixture"
	projectRoot := filepath.Join(state, "projects", project)
	if got := lease.New(projectRoot).Acquire(owner, task, []string{"src"}, lease.AcquireOptions{}, float64(time.Now().Unix())); got.Status != "OK" {
		t.Fatalf("acquire lease: %#v", got)
	}
	call := "fixture call"
	jobID := jobHash(call)
	original := map[string]any{"session_id": "session", "tool_use_id": "origin", "tool_name": "mcp__codex-main__codex"}
	key, err := callKey(original)
	if err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"session_id": "session", "tool_use_id": "result", "tool_name": "mcp__codex-main__codex_result",
		"hook_event_name": "PostToolUse", "tool_input": map[string]any{"jobId": jobID}, "tool_response": map[string]any{"structuredContent": map[string]any{
			"jobId": jobID, "state": "SUCCEEDED", "threadId": "thread-fixture", "content": "done"}}}
	index := map[string]any{"jobId": jobID, "call": call, "call_key": key, "project_key": project, "session_id": "session", "stage": "FINALIZING"}
	pending := map[string]any{"task_id": task, "phase": "implementation", "scope": []any{"src"}, "baseline": baseline,
		"lease_owner": owner, "project_key": project, "session_id": "session", "route": "normal", "model": "openai/gpt-5",
		"effort": "medium", "task_contract_grounded": true, "worker_validation": map[string]any{"status": "PASS", "summary": "passed"},
		"task_objective": "review the async result", "acceptance_criteria": "record the outcome",
		"tool_use_id": "origin", "job_id": jobID, "async_call": call}
	job := map[string]any{"jobId": jobID, "call": call, "state": "SUCCEEDED", "threadId": "thread-fixture", "repoRoot": repo}
	if err = atomicAdmissionJSON(filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), job); err != nil {
		t.Fatal(err)
	}
	response, err := finishAsyncSuccess(state, event, index, pending, original, repo, map[string]any{"post_tool_output_field": "updatedToolOutput"},
		postProfile{Stored: true, ProtectedPaths: []string{}, SkipMaxFiles: intAddr(0), SkipMaxLines: intAddr(0)}, nil,
		map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || index["stage"] != "CONSUMED" {
		t.Fatalf("async result not consumed: response=%#v index=%#v", response, index)
	}
	if bundle := responseBundle(t, response); bundle["claude_review_mode"] != "AUDIT" || bundle["reviewer1_skipped_reason"] != "NO_DELTA" {
		t.Fatalf("no-delta review = %#v", bundle)
	}
	lock, _, _, _, cached, err := claimAsyncResult(state, event, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseTaskUsageLock(lock)
	if cached == nil {
		t.Fatal("consumed result did not return its cached response")
	}
	badEvent := map[string]any{}
	for key, value := range event {
		badEvent[key] = value
	}
	badEvent["tool_response"] = map[string]any{"structuredContent": map[string]any{
		"jobId": jobID, "state": "FAILED", "threadId": "thread-fixture", "content": "done"}}
	if _, err = finishAsyncSuccess(state, badEvent, index, pending, original, repo,
		map[string]any{"post_tool_output_field": "updatedToolOutput"}, postProfile{}, nil, nil, 8000, 1200, time.Second); err == nil {
		t.Fatal("failed/mismatched Codex result was accepted as successful")
	}
}

func runAsyncSuccessGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func responseBundle(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	output := mapValue(response["hookSpecificOutput"])["updatedToolOutput"].([]any)[0].(map[string]any)
	var bundle map[string]any
	if err := json.Unmarshal([]byte(stringValue(output["text"])), &bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func intAddr(value int) *int { return &value }
