package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/ledger"
	"purser-hook/internal/review"
)

func TestPostToolUseSyncBypassDispatch(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	root := t.TempDir()
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex",
		"tool_use_id": "dispatch-sync", "session_id": "dispatch-session",
		"tool_response": map[string]any{"structuredContent": map[string]any{"threadId": "thread-dispatch", "content": "done"}}}
	if err = writeFixtureJSON(bypassPath(root, event), map[string]any{"kind": "pretool_bypass", "reason": "test bypass"}); err != nil {
		t.Fatal(err)
	}
	response, failure := PostToolUse(event, root)
	if failure != nil {
		t.Fatal(failure)
	}
	bundle := responseBundle(t, response)
	if bundle["thread_id"] != "thread-dispatch" || bundle["routing_enforced"] != false || bundle["claude_review_mode"] != "FULL" {
		t.Fatalf("unexpected sync bundle: %#v", bundle)
	}
	if _, err = os.Stat(stringValue(bundle["raw_output_path"])); err != nil {
		t.Fatalf("raw output not persisted: %v", err)
	}
	if _, err = os.Stat(bypassPath(root, event)); !os.IsNotExist(err) {
		t.Fatalf("bypass pending was not removed: %v", err)
	}
}

func TestPostToolUseReviewerBridgeDispatch(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	root, repo := t.TempDir(), t.TempDir()
	if err = os.MkdirAll(filepath.Join(repo, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	runAsyncSuccessGit(t, repo, "init", "-q")
	for i := 0; i < 3; i++ {
		path := filepath.Join(repo, "src", string(rune('a'+i))+".go")
		if err = os.WriteFile(path, []byte("package src\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runAsyncSuccessGit(t, repo, "add", "--all")
	runAsyncSuccessGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "baseline")
	baseline := ledger.CaptureBaseline(repo, []string{"src"}, 1)
	if baseline["status"] != ledger.OK {
		t.Fatalf("baseline: %#v", baseline)
	}
	if err = os.WriteFile(filepath.Join(repo, "src", "a.go"), []byte("package src\n// changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, "src", "b.go"), []byte("package src\n// changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, "src", "c.go"), []byte("package src\n// changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex", "tool_use_id": "dispatch-review",
		"session_id": "dispatch-review-session", "tool_response": map[string]any{"structuredContent": map[string]any{"threadId": "thread-review", "content": "done"}}}
	pending := map[string]any{"kind": "call", "task_id": "dispatch-review-task", "phase": "implementation", "scope": []any{"src"}, "baseline": baseline,
		"project_key": "dispatch-review-project", "repo_root": repo, "session_id": event["session_id"], "tool_use_id": event["tool_use_id"],
		"route": "normal", "model": "openai/gpt-5", "effort": "medium", "task_contract_grounded": true, "agent": "codex",
		"task_objective": "verify post-call handoff", "acceptance_criteria": "session ledger records both baselines",
		"protected_paths": []any{}, "force_full_review": false, "skip_max_lines": 50, "skip_max_files": 2, "configured": true,
		"user_status": "accepted", "project_status": "accepted", "reviewer_candidates": []any{map[string]any{"agent": "agy", "model": "google/gemini-3.8-flash-high"}}}
	if err = writeFixtureJSON(mustPendingPath(t, root, event), pending); err != nil {
		t.Fatal(err)
	}
	old := postReviewerDispatch
	postReviewerDispatch = func(_ string, _ string, _ map[string]any, _ map[string]any, _ postProfile, _ map[string]any,
		_ map[string]any, _ map[string]any, _ time.Time, _ time.Duration) (review.BridgeOutcome, error) {
		return review.BridgeOutcome{Outcome: "completed", Attributes: map[string]any{"review_status": "COMPLETED", "result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE",
			"validation": "PASS", "provider": "agy", "reviewer1_model_requested": "google/gemini-3.8-flash-high", "findings": []any{}}}, nil
	}
	t.Cleanup(func() { postReviewerDispatch = old })
	response, failure := PostToolUse(event, root)
	if failure != nil {
		t.Fatal(failure)
	}
	bundle := responseBundle(t, response)
	if bundle["reviewer1_result"] != "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" || bundle["thread_id"] != "thread-review" {
		t.Fatalf("review dispatch bundle: %#v", bundle)
	}
	sessionPath, err := ledger.PathForTask(filepath.Join(root, "projects", "dispatch-review-project"), "dispatch-review-task")
	if err != nil {
		t.Fatal(err)
	}
	loaded := ledger.Load(sessionPath, "dispatch-review-task")
	if loaded.Status != ledger.OK {
		t.Fatalf("session load: %+v findings=%#v", loaded, bundle["findings"])
	}
	thread := mapValue(loaded.Record["live_thread"])
	if mapValue(thread["origin_baseline"])["status"] != ledger.OK || mapValue(thread["post_state"])["status"] != ledger.OK || thread["origin_scope"] == nil {
		t.Fatalf("post-call baseline handoff missing: %#v", thread)
	}
	if _, err = os.Stat(stringValue(bundle["full_bundle_path"])); err != nil {
		t.Fatalf("bundle artifact missing: %v", err)
	}
}

func TestPostToolUseDispatchStartApplyAndUnknown(t *testing.T) {
	root := t.TempDir()
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	unknown, err := PostToolUse(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "other"}, root)
	if err != nil || len(unknown) != 0 {
		t.Fatalf("unknown tool response=%#v err=%v", unknown, err)
	}
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_start",
		"tool_use_id": "dispatch-start", "session_id": "dispatch-session",
		"tool_response": map[string]any{"structuredContent": map[string]any{"jobId": "bad", "state": "RUNNING"}}}
	if err = writeFixtureJSON(mustPendingPath(t, root, event), map[string]any{"async_ready": true, "async_call": "call", "tool_use_id": "dispatch-start", "session_id": "dispatch-session"}); err != nil {
		t.Fatal(err)
	}
	start, failure := PostToolUse(event, root)
	if failure != nil {
		t.Fatal(failure)
	}
	if start == nil || mapValue(start["hookSpecificOutput"]) == nil {
		t.Fatalf("start was not finalized: %#v", start)
	}
	apply, failure := PostToolUse(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_apply",
		"tool_input": map[string]any{"jobId": "missing"}}, root)
	if failure != nil || len(apply) != 0 {
		t.Fatalf("apply response=%#v err=%v", apply, failure)
	}
	for _, tool := range []string{"mcp__codex-main__codex_result", "mcp__codex-main__codex_cancel"} {
		response, failure := PostToolUse(map[string]any{"hook_event_name": "PostToolUse", "tool_name": tool,
			"session_id": "dispatch-session", "tool_use_id": "missing-" + tool, "tool_input": map[string]any{"jobId": "missing"}}, root)
		if failure != nil || response == nil {
			t.Fatalf("%s response=%#v err=%v", tool, response, failure)
		}
		if _, ok := response["hookSpecificOutput"]; !ok {
			t.Fatalf("%s returned non-hook-shaped interruption: %#v", tool, response)
		}
	}
}

func TestPostToolUseDispatchOffersCachedCodexModelUpgrade(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	state, codexHome, repo := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("CODEX_HOME", codexHome)
	configBytes, err := os.ReadFile(filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shippedConfig map[string]any
	if err := json.Unmarshal(configBytes, &shippedConfig); err != nil {
		t.Fatal(err)
	}
	currentRouteModel := stringValue(mapValue(mapValue(shippedConfig["routes"])["normal"])["model"])
	if currentRouteModel == "" {
		t.Fatal("shipped normal route model is missing")
	}
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(
		`{"models":[{"slug":"gpt-6.2-sol","visibility":"list"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_start",
		"tool_use_id": "model-upgrade-dispatch", "session_id": "model-upgrade-session"}
	call := "model-upgrade-call"
	jobID := jobHash(call)
	event["tool_response"] = map[string]any{"structuredContent": map[string]any{"jobId": jobID, "state": "RUNNING"}}
	pending := map[string]any{"async_ready": true, "async_call": call, "tool_use_id": event["tool_use_id"],
		"session_id": event["session_id"], "repo_root": repo, "project_key": "model-upgrade-project",
		"route": "normal", "model": currentRouteModel, "effort": "high", "phase": "implementation"}
	if err := writeFixtureJSON(mustPendingPath(t, state, event), pending); err != nil {
		t.Fatal(err)
	}
	response, failure := PostToolUse(event, state)
	if failure != nil {
		t.Fatal(failure)
	}
	output := mapValue(response["hookSpecificOutput"])
	context := stringValue(output["additionalContext"])
	if !strings.Contains(context, "New model version available: `gpt-6.2-sol`") || !strings.Contains(context, currentRouteModel) {
		t.Fatalf("model upgrade guidance missing from dispatch output: %q", context)
	}
	stateData, err := os.ReadFile(filepath.Join(state, "model-version-notice.json"))
	if err != nil {
		t.Fatal(err)
	}
	var notice map[string]any
	if err := json.Unmarshal(stateData, &notice); err != nil {
		t.Fatal(err)
	}
	queued := mapValue(notice["pending"])
	if queued["current"] != currentRouteModel || queued["new"] != "gpt-6.2-sol" {
		t.Fatalf("pending model notice = %#v", queued)
	}
}

func TestPostDispatchModelCacheFailuresLeaveOutputUnchanged(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	for _, tc := range []struct {
		name  string
		cache *string
	}{
		{name: "missing"},
		{name: "malformed", cache: testStringPointer("{")},
		{name: "hidden only", cache: testStringPointer(`{"models":[{"slug":"gpt-6.1-sol","visibility":"hide"}]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, codexHome := t.TempDir(), t.TempDir()
			t.Setenv("PURSER_STATE_DIR", state)
			t.Setenv("CODEX_HOME", codexHome)
			if tc.cache != nil {
				if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(*tc.cache), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pending := map[string]any{"async_ready": true, "async_call": "existing-call", "route": "normal",
				"model": "gpt-6-sol", "effort": "medium", "task_id": "cache-failure"}
			baseEvent := map[string]any{"tool_name": "mcp__codex-main__codex", "session_id": "cache-failure-session"}
			want := appendPostDispatchReceipt(map[string]any{}, state, baseEvent, pending, "")
			startEvent := map[string]any{"tool_name": "mcp__codex-main__codex_start", "session_id": "cache-failure-session"}
			got := appendPostDispatchReceipt(map[string]any{}, state, startEvent, pending, "")
			wantContext := stringValue(mapValue(want["hookSpecificOutput"])["additionalContext"])
			gotContext := stringValue(mapValue(got["hookSpecificOutput"])["additionalContext"])
			if gotContext != wantContext {
				t.Fatalf("output changed: got %q want %q", gotContext, wantContext)
			}
		})
	}
}

func testStringPointer(value string) *string { return &value }

func TestPostToolUseAsyncResultDispatch(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	state, repo := t.TempDir(), t.TempDir()
	if err = os.MkdirAll(filepath.Join(repo, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	runAsyncSuccessGit(t, repo, "init", "-q")
	if err = os.WriteFile(filepath.Join(repo, "src", "file.go"), []byte("package src\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAsyncSuccessGit(t, repo, "add", "--all")
	runAsyncSuccessGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
	baseline := ledger.CaptureBaseline(repo, []string{"src"}, 1)
	if baseline["status"] != ledger.OK {
		t.Fatalf("baseline: %#v", baseline)
	}
	original := map[string]any{"tool_name": "mcp__codex-main__codex", "session_id": "dispatch-session", "tool_use_id": "dispatch-origin"}
	key, err := callKey(original)
	if err != nil {
		t.Fatal(err)
	}
	call, jobID, project := "dispatch-call", jobHash("dispatch-call"), "dispatch-project"
	pending := map[string]any{"task_id": "dispatch-task", "phase": "implementation", "scope": []any{"src"}, "baseline": baseline,
		"project_key": project, "repo_root": repo, "session_id": "dispatch-session", "tool_use_id": "dispatch-origin", "job_id": jobID,
		"async_call": call, "route": "normal", "model": "openai/gpt-5", "effort": "medium", "task_contract_grounded": true,
		"worker_validation": map[string]any{"source": "WORKER_REPORTED", "status": "PASS", "summary": "passed"}}
	if err = writeFixtureJSON(filepath.Join(state, "pending", key+".json"), pending); err != nil {
		t.Fatal(err)
	}
	index := map[string]any{"jobId": jobID, "call": call, "call_key": key, "project_key": project, "session_id": "dispatch-session", "stage": "READY"}
	if err = writeFixtureJSON(filepath.Join(state, "async_jobs", jobID+".json"), index); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"jobId": jobID, "call": call, "state": "SUCCEEDED", "threadId": "thread-dispatch", "repoRoot": repo}
	if err = writeFixtureJSON(filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), job); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_result", "session_id": "dispatch-session",
		"tool_use_id": "dispatch-result", "tool_input": map[string]any{"jobId": jobID},
		"tool_response": map[string]any{"structuredContent": map[string]any{"jobId": jobID, "state": "SUCCEEDED", "threadId": "thread-dispatch", "content": "done"}}}
	response, failure := PostToolUse(event, state)
	if failure != nil {
		t.Fatal(failure)
	}
	if response == nil {
		t.Fatal("async result returned no response")
	}
	if got := responseBundle(t, response); got["thread_id"] != "thread-dispatch" {
		t.Fatalf("async result bundle: %#v", got)
	}
}

func TestPostAsyncDispatchWrapsEmptyCachedResponse(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	root := t.TempDir()
	call, jobID := "cached response fixture", jobHash("cached response fixture")
	event := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_result",
		"session_id": "cached-session", "tool_input": map[string]any{"jobId": jobID}}
	index := map[string]any{"jobId": jobID, "call": call, "call_key": jobID, "project_key": "cached-project",
		"session_id": "cached-session", "stage": "CONSUMED", "response": ""}
	if err = writeFixtureJSON(filepath.Join(root, "async_jobs", jobID+".json"), index); err != nil {
		t.Fatal(err)
	}
	response, err := postAsyncDispatch(event, root)
	if err != nil {
		t.Fatal(err)
	}
	if mapValue(response["hookSpecificOutput"]) == nil {
		t.Fatalf("cached fallback is not replacement-wrapped: %#v", response)
	}
}

func TestFailedAsyncFinalizationIsTerminalAndReturnsCachedError(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	state, repo, event, index, pending, original, _, _, _ := newAsyncSuccessCase(t, "forced_finalize_failure")
	pending["scope"] = "malformed"
	if err = writeAsyncObject(pendingPathForAsync(state, original), pending); err != nil {
		t.Fatal(err)
	}
	response, err := postAsyncDispatch(event, state)
	if err != nil || response == nil {
		t.Fatalf("failed finalization response=%#v err=%v", response, err)
	}
	stored, err := readAsyncObject(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"))
	if err != nil || stored["stage"] != "FAILED" || stored["errorCode"] != "INTERNAL_POST_ERROR" || stored["claude_review_mode"] != review.Full || !reflect.DeepEqual(stored["response"], response) {
		t.Fatalf("terminal failure index=%#v err=%v", stored, err)
	}
	if bundle := postResponseBundle(response, map[string]any{"post_tool_output_field": "updatedToolOutput"}); bundle["claude_review_mode"] != review.Full {
		t.Fatalf("terminal failure did not force FULL review: %#v", bundle)
	}
	repeated, err := postAsyncDispatch(event, state)
	if err != nil || !reflect.DeepEqual(repeated, response) {
		t.Fatalf("repeat result=%#v err=%v, want cached %#v", repeated, err, response)
	}
	_ = repo
}

func mustPendingPath(t *testing.T, root string, event map[string]any) string {
	t.Helper()
	path, err := pendingPath(root, event)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
