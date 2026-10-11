package enforce

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
	"purser-hook/internal/policy"
	"purser-hook/internal/review"
)

func TestCodexHookPortMechanicalOpenCodeCandidateSelection(t *testing.T) {
	config := map[string]any{"coder_agent": "codex", "mechanical_opencode_enabled": true}
	candidate := func(id string, passed int) any {
		return map[string]any{"id": id, "status": "unmeasured", "benchmarks": map[string]any{
			"1": map[string]any{"passed_count": passed, "total": 6},
		}}
	}
	for _, tc := range []struct {
		name       string
		candidates []any
		wantAgent  string
		wantModel  string
	}{
		{name: "qualified score binds candidate", candidates: []any{candidate("opencode/nemotron-3-ultra-free", 5)}, wantAgent: "opencode", wantModel: "opencode/nemotron-3-ultra-free"},
		{name: "below threshold keeps configured coder", candidates: []any{candidate("vendor/weak", 4)}, wantAgent: "codex", wantModel: "gpt-6-luna"},
		{name: "empty inventory keeps configured coder", wantAgent: "codex", wantModel: "gpt-6-luna"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectCoder("mechanical", "gpt-6-luna", "high", "new_task", true, false,
				config, nil, nil, nil, tc.candidates, false)
			if got.Agent != tc.wantAgent || got.Model != tc.wantModel {
				t.Fatalf("selection = %#v, want agent=%q model=%q", got, tc.wantAgent, tc.wantModel)
			}
		})
	}

	optOut := map[string]any{"coder_agent": "codex", "mechanical_opencode_enabled": false}
	if got := SelectCoder("mechanical", "gpt-6-luna", "high", "new_task", true, false,
		optOut, nil, nil, nil, []any{candidate("vendor/seeded", 6)}, false); got.Agent != "codex" || got.Model != "gpt-6-luna" {
		t.Fatalf("opt-out selection = %#v", got)
	}
	if got := SelectCoder("normal", "gpt-6-sol", "medium", "new_task", true, false,
		config, nil, nil, nil, []any{candidate("vendor/seeded", 6)}, false); got.Agent != "codex" || got.Model != "gpt-6-sol" {
		t.Fatalf("nonmechanical selection = %#v", got)
	}
	configuredOpenCode := map[string]any{"coder_agent": "opencode"}
	if got := SelectCoder("mechanical", "provider/global", "none", "new_task", true, false,
		configuredOpenCode, nil, nil, nil, nil, false); got.Agent != "opencode" || got.Model != "provider/global" {
		t.Fatalf("empty candidates changed configured OpenCode binding: %#v", got)
	}
	if got := SelectCoder("mechanical", "gpt-6-luna", "high", "", false, true,
		config, nil, map[string]any{"agent": "codex", "model": "thread-model", "effort": "high"}, nil,
		[]any{candidate("vendor/seeded", 6)}, false); got.Model != "thread-model" {
		t.Fatalf("reply changed recorded model: %#v", got)
	}
	if got := SelectCoder("normal", "provider/global", "none", "continue", false, true,
		configuredOpenCode, map[string]any{}, map[string]any{"agent": "codex", "model": "foreign-model"}, nil, nil, false); got.Code != "THREAD_AGENT_MISMATCH" {
		t.Fatalf("cross-agent reply was admitted: %#v", got)
	}
}

func TestCodexHookPortBoardStatesAndRetention(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1800000000, 0)
	writeJob := func(id, label, state string, accepted float64, values map[string]any, bundle map[string]any) {
		t.Helper()
		call, callKey := "call-"+id, "key-"+id
		indexDir := filepath.Join(root, "async_jobs")
		jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
		if err := os.MkdirAll(indexDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(jobDir, 0700); err != nil {
			t.Fatal(err)
		}
		write := func(path string, value any) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(indexDir, id+".json"), map[string]any{"project_key": "project", "jobId": id, "call": call, "call_key": callKey})
		job := map[string]any{"jobId": id, "call": call, "state": state, "acceptedAt": accepted, "label": label,
			"model": "gpt-6-sol", "effort": "medium", "route": "normal", "boardDisplayCount": json.Number("0")}
		for key, value := range values {
			job[key] = value
		}
		write(filepath.Join(jobDir, id+".json"), job)
		if bundle != nil {
			bundleDir := filepath.Join(root, "bundle_artifacts")
			if err := os.MkdirAll(bundleDir, 0700); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(bundleDir, callKey+".json"), bundle)
		}
	}
	writeJob(strings.Repeat("a", 64), "stalled", "RUNNING", float64(now.Unix()-120), map[string]any{
		"errorCode": "STALLED", "stallDetectedAt": float64(now.Unix() - 30), "startedAt": float64(now.Unix() - 30), "etaMinutes": json.Number("5"),
	}, nil)
	writeJob(strings.Repeat("b", 64), "unlabelled", "RUNNING", float64(now.Unix()), map[string]any{"label": ""}, nil)
	writeJob(strings.Repeat("c", 64), "no-change", "SUCCEEDED", float64(now.Unix()-30), nil, map[string]any{
		"phase": "implementation", "apply": "codex_apply", "changed_paths": []any{},
	})
	writeJob(strings.Repeat("d", 64), "pending", "SUCCEEDED", float64(now.Unix()-20), nil, map[string]any{
		"phase": "implementation", "apply": "codex_apply", "changed_paths": []any{"file.go"},
	})
	writeJob(strings.Repeat("e", 64), "expired", "FAILED", float64(now.Unix()-4000), map[string]any{
		"finishedAt": float64(now.Unix() - 4000),
	}, nil)
	writeJob(strings.Repeat("f", 64), "already-shown", "SUCCEEDED", float64(now.Unix()-10), map[string]any{
		"finishedAt": float64(now.Unix() - 10), "boardDisplayCount": json.Number("2"),
	}, nil)
	writeJob(strings.Repeat("1", 64), "default-eta", "RUNNING", float64(now.Unix()-10), map[string]any{
		"startedAt": float64(now.Unix() - 10),
	}, nil)
	writeJob(strings.Repeat("2", 64), "mechanical-eta", "RUNNING", float64(now.Unix()), map[string]any{
		"route": "mechanical", "startedAt": float64(now.Unix()),
	}, nil)
	writeJob(strings.Repeat("3", 64), "queued-old", "QUEUED", float64(now.Unix()-3600), map[string]any{
		"etaMinutes": json.Number("5"),
	}, nil)

	board, ids := PostProjectJobBoard(root, "project", now)
	for _, want := range []string{
		"| stalled | gpt-6-sol (medium) | stalled | ~5 min |",
		"| no-change | gpt-6-sol (medium) | done | — | — | no change |",
		"| pending | gpt-6-sol (medium) | in review | — | — | — |",
		"| default-eta | gpt-6-sol (medium) | coding | ~5 min |",
		"| mechanical-eta | gpt-6-sol (medium) | coding | ~3 min |",
		"| queued-old | gpt-6-sol (medium) | queued | — |",
	} {
		if !strings.Contains(board, want) {
			t.Errorf("board lacks %q:\n%s", want, board)
		}
	}
	for _, absent := range []string{"unlabelled", "expired", "already-shown"} {
		if strings.Contains(board, absent) {
			t.Errorf("board unexpectedly includes %q:\n%s", absent, board)
		}
	}
	if len(ids) != 6 {
		t.Fatalf("visible job ids = %v, want 6", ids)
	}
}

func TestCodexHookPortStaleAsyncPendingKeepsBaselineWithoutLease(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "pending", "old.json")
	mustPostTest(t, writeAsyncObject(path, map[string]any{"job_id": strings.Repeat("a", 64), "baseline": map[string]any{"status": "OK"}}))
	old := time.Now().Add(-25 * time.Hour)
	mustPostTest(t, os.Chtimes(path, old, old))
	cleanupStaleFailurePending(state, map[string]any{"session_id": "s", "tool_use_id": "other"}, "other")
	if !isFile(path) {
		t.Fatal("stale async baseline without lease_owner was removed")
	}
}

func TestCodexHookPortCompactWholeTreeAndConcurrentSummary(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		bundle := baseBundle("")
		bundle["whole_tree_delta"], bundle["whole_tree_anomaly"] = map[string]any{"changed_paths": []string{"file.go"}}, map[string]any{"paths": []string{"file.go"}}
		if concurrent {
			scopes := []any{}
			for i := 0; i < 8; i++ {
				scopes = append(scopes, map[string]any{"path": fmt.Sprintf("file%d.go", i), "task_id": fmt.Sprintf("task-%d", i)})
			}
			bundle["known_concurrent_scopes"] = scopes
		}
		compact, overflow, err := compactBundle(bundle, 8000)
		mustPostTest(t, err)
		if overflow || compact["whole_tree_delta"] != nil || compact["whole_tree_anomaly"] != nil {
			t.Fatalf("whole-tree fields leaked: %#v", compact)
		}
		if concurrent {
			summary := mapValue(compact["known_concurrent_scopes"])
			if summary["count"] != 8 || strings.Join(anyStringList(summary["task_ids"]), ",") != "task-0,task-1,task-2,task-3,task-4" {
				t.Fatalf("concurrent summary = %#v", summary)
			}
		} else if _, exists := compact["known_concurrent_scopes"]; exists {
			t.Fatal("single-job compact bundle has concurrent scopes")
		}
		_, path, err := finalizePostArtifacts(t.TempDir(), map[string]any{"session_id": "s", "tool_use_id": "u"}, bundle, 8000, 1200)
		mustPostTest(t, err)
		full, err := readAsyncObject(path)
		mustPostTest(t, err)
		if full["whole_tree_delta"] == nil || full["whole_tree_anomaly"] == nil || len(full["known_concurrent_scopes"].([]any)) != len(bundle["known_concurrent_scopes"].([]any)) {
			t.Fatalf("artifact lost whole-tree evidence: %#v", full)
		}
	}
}

func TestCodexHookPortReviewerScopePreview(t *testing.T) {
	paths := []string{}
	for i := 0; i < 14; i++ {
		paths = append(paths, fmt.Sprintf("src/path%d.go", i))
	}
	preview := postPathPreview(paths)
	if strings.Join(preview[:10], ",") != strings.Join(paths[:10], ",") || len(preview) != 11 || preview[10] != "+4 more" {
		t.Fatalf("scope preview = %v", preview)
	}
}

func TestCodexHookPortTrailingValidationCompaction(t *testing.T) {
	for _, valid := range []bool{true, false} {
		record := `VALIDATION: {"source":"WORKER_REPORTED","status":"PASS","commands":["pytest"],"exit_codes":[0]}`
		if !valid {
			record = "VALIDATION: {bad json}"
		}
		report := "Implemented the change.\n" + record
		bundle := baseBundle("")
		bundle["implementer_report"], bundle["worker_validation"] = report, parseWorkerValidation(report)
		compact, path, err := finalizePostArtifacts(t.TempDir(), map[string]any{"session_id": "s", "tool_use_id": "u"}, bundle, 8000, 1200)
		mustPostTest(t, err)
		want := report
		if valid {
			want = "Implemented the change."
		}
		full, err := readAsyncObject(path)
		mustPostTest(t, err)
		if compact["implementer_report"] != want || bundle["implementer_report"] != report || full["implementer_report"] != report {
			t.Fatalf("validation compaction changed original or kept wrong line: %#v", compact)
		}
	}
}

func codexHookPortWorkspace(t *testing.T) (string, string) {
	t.Helper()
	oldCompletion := startCodeMapCompletionDetached
	startCodeMapCompletionDetached = func([]string, []string, string) error { return nil }
	t.Cleanup(func() { startCodeMapCompletionDetached = oldCompletion })
	oldBridge := postReviewerBridge
	postReviewerBridge = func(_ context.Context, _ string, _ map[string]any, _ time.Duration) (review.BridgeOutcome, error) {
		return review.BridgeOutcome{Outcome: "completed", Attributes: map[string]any{"result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "validation": "PASS", "findings": []any{}}}, nil
	}
	t.Cleanup(func() { postReviewerBridge = oldBridge })
	repo, state := postDeltaRepo(t), t.TempDir()
	plugin, err := filepath.Abs(filepath.Join("..", "..", ".."))
	mustPostTest(t, err)
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	return repo, state
}

func codexHookPortCall(repo, id, tool, prompt string) map[string]any {
	return map[string]any{"session_id": "port-session", "tool_use_id": id, "tool_name": tool, "cwd": repo,
		"hook_event_name": "PreToolUse", "tool_input": map[string]any{"cwd": repo, "prompt": prompt, "label": id}}
}

func TestCodexHookPortCodeMapPromptPreservedWithoutDuplication(t *testing.T) {
	for _, line := range []string{`CODE_MAP=C:\Users\Example User\code-map\map.txt STATUS=FRESH`, "CODE_MAP=/existing/map.txt STATUS=STALE"} {
		repo, state := codexHookPortWorkspace(t)
		call := codexHookPortCall(repo, "map-preserved", "mcp__codex-main__codex", "ORCH|task=map-preserved|phase=implementation|route=subtle|scope=scope\nDo the work.\n"+line)
		response, err := Pre(call, state)
		mustPostTest(t, err)
		prompt := stringValue(mapValue(mapValue(response["hookSpecificOutput"])["updatedInput"])["prompt"])
		if strings.Count(prompt, line) != 1 || strings.Count(prompt, "CODE_MAP=") != 1 {
			t.Fatalf("code-map pointer changed or duplicated: %q", prompt)
		}
	}
}

func TestCodexHookPortCodeMapInjectedForImplementationAndPlan(t *testing.T) {
	for _, phase := range []string{"implementation", "plan"} {
		repo, state := codexHookPortWorkspace(t)
		route := "subtle"
		if phase == "plan" {
			route = "plan_standard"
		}
		call := codexHookPortCall(repo, "map-injected", "mcp__codex-main__codex", "ORCH|task=map-injected|phase="+phase+"|route="+route+"|scope=scope\nDo the work.")
		response, err := Pre(call, state)
		mustPostTest(t, err)
		prompt := stringValue(mapValue(mapValue(response["hookSpecificOutput"])["updatedInput"])["prompt"])
		if strings.Count(prompt, "CODE_MAP=") != 1 || !strings.Contains(prompt, "STATUS=FRESH") {
			t.Fatalf("%s prompt lacks fresh code map: %q", phase, prompt)
		}
	}
}

func TestCodexHookPortCodeMapFailureContinuesDispatch(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	dir := filepath.Join(state, "projects", projectKey(&repo))
	mustPostTest(t, os.MkdirAll(dir, 0700))
	mustPostTest(t, os.WriteFile(filepath.Join(dir, "code-map"), []byte("blocked"), 0600))
	call := codexHookPortCall(repo, "map-unavailable", "mcp__codex-main__codex", "ORCH|task=map-unavailable|phase=implementation|route=subtle|scope=scope\nDo the work.")
	response, err := Pre(call, state)
	mustPostTest(t, err)
	output := mapValue(response["hookSpecificOutput"])
	prompt := stringValue(mapValue(output["updatedInput"])["prompt"])
	if output["permissionDecision"] != nil || !strings.Contains(prompt, "CODE_MAP=") || !strings.HasSuffix(prompt, "STATUS=UNAVAILABLE") {
		t.Fatalf("map failure blocked dispatch: %#v", response)
	}
}

func TestCodexHookPortCodeMapStaleAfterMutation(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		repo, state := codexHookPortWorkspace(t)
		prompt := "ORCH|task=map-mutation|phase=implementation|route=subtle|scope=scope\n## Objective\nUpdate scope.\n## Acceptance criteria\n- scope changes."
		if supplied {
			prompt += "\nCODE_MAP=C:\\state\\map.txt STATUS=FRESH"
		}
		call := codexHookPortCall(repo, "map-mutation", "mcp__codex-main__codex", prompt)
		_, err := Pre(call, state)
		mustPostTest(t, err)
		mustPostTest(t, os.WriteFile(filepath.Join(repo, "scope", "file.txt"), []byte("changed\n"), 0600))
		call["hook_event_name"], call["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "map-thread", "content": "done"}}
		response, err := PostToolUse(call, state)
		mustPostTest(t, err)
		if responseBundle(t, response)["code_map"] != "STALE" {
			t.Fatalf("mutation did not mark code map stale: %#v", response)
		}
	}
}

func TestCodexHookPortCorruptLeaseDeniesAdmission(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	dir := filepath.Join(state, "projects", projectKey(&repo))
	mustPostTest(t, os.MkdirAll(dir, 0700))
	mustPostTest(t, os.WriteFile(filepath.Join(dir, "scope_leases.json"), []byte("{broken"), 0600))
	response, err := Pre(codexHookPortCall(repo, "corrupt-lease", "mcp__codex-main__codex", "ORCH|task=corrupt-lease|phase=implementation|route=subtle|scope=scope\nDo work."), state)
	mustPostTest(t, err)
	if !strings.Contains(stringValue(mapValue(response["hookSpecificOutput"])["permissionDecisionReason"]), "LEASE_ADMISSION_UNCERTAIN") {
		t.Fatalf("corrupt lease admitted: %#v", response)
	}
}

func TestCodexHookPortSyncLeaseDeadline(t *testing.T) {
	root := t.TempDir()
	store := lease.New(root)
	if got := store.Acquire("old", "old-task", []string{"file.go"}, lease.AcquireOptions{TimeoutSeconds: 10}, 1000); got.Status != "OK" {
		t.Fatal(got)
	}
	if got := store.Acquire("new", "new-task", []string{"file.go"}, lease.AcquireOptions{}, 1069); got.Code != "SCOPE_LEASE_CONFLICT" {
		t.Fatal(got)
	}
	if got := store.Acquire("new", "new-task", []string{"file.go"}, lease.AcquireOptions{}, 1070); got.Status != "OK" {
		t.Fatal(got)
	}
	value, err := readAsyncObject(filepath.Join(root, "scope_leases.json"))
	mustPostTest(t, err)
	owners := mapValue(value["owners"])
	if mapValue(owners["old"])["state"] != "RELEASED" || numberOr(mapValue(owners["new"])["deadline_at"], 0) != 1070+7200 {
		t.Fatalf("deadline reconciliation = %#v", owners)
	}
}

func TestCodexHookPortQuotaWindowFormatting(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("fixture", -7*3600)
	t.Cleanup(func() { time.Local = oldLocal })
	quota := map[string]any{"5h_used_pct": float64(1), "5h_resets_at": "2026-09-25T08:57:11Z", "7d_used_pct": float64(46), "7d_resets_at": "2026-09-28T16:07:43Z"}
	message := "⚓ Purser: Codex quota left: 5h 99% (resets 01:57) · 7d 54% (resets Sep 28)"
	if got := postQuotaSystemMessage(quota); got != message {
		t.Fatalf("local quota message = %q", got)
	}
	response, err := replacement(baseBundle(""), "updatedToolOutput")
	mustPostTest(t, err)
	before, err := canonicalJSON(response["hookSpecificOutput"])
	mustPostTest(t, err)
	appendPostQuotaSystemMessage(response, quota)
	after, err := canonicalJSON(response["hookSpecificOutput"])
	mustPostTest(t, err)
	if response["systemMessage"] != message || string(before) != string(after) || len(response) != 2 {
		t.Fatalf("quota changed replacement: %#v", response)
	}
}

func TestCodexHookPortQuotaMalformedWindows(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })
	for _, quota := range []any{nil, map[string]any{}, map[string]any{"5h_used_pct": float64(1), "5h_resets_at": "not-a-timestamp"}} {
		if got := postQuotaSystemMessage(quota); got != "" {
			t.Fatalf("malformed quota rendered: %q", got)
		}
	}
	for _, invalid := range []bool{false, true} {
		quota := map[string]any{"7d_used_pct": float64(46), "7d_resets_at": "2026-09-28T16:07:43Z"}
		if invalid {
			quota["5h_used_pct"], quota["5h_resets_at"] = float64(1), "not-a-timestamp"
		}
		if got := postQuotaSystemMessage(quota); got != "⚓ Purser: Codex quota left: 7d 54% (resets Sep 28)" {
			t.Fatalf("valid remaining window lost: %q", got)
		}
	}
}

func TestCodexHookPortQuotaRemainingPercentClamped(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })
	for _, tc := range []struct {
		used float64
		left int
	}{{100.4, 0}, {-3, 100}} {
		got := postQuotaSystemMessage(map[string]any{"5h_used_pct": tc.used, "5h_resets_at": "2026-09-25T08:57:11Z"})
		if want := fmt.Sprintf("⚓ Purser: Codex quota left: 5h %d%% (resets 08:57)", tc.left); got != want {
			t.Fatalf("clamped message = %q want %q", got, want)
		}
	}
}

func TestCodexHookPortQuotaJoinsExistingMessage(t *testing.T) {
	response := map[string]any{"systemMessage": "Existing message"}
	quota := map[string]any{"7d_used_pct": float64(46), "7d_resets_at": "2026-09-28T16:07:43Z"}
	appendPostQuotaSystemMessage(response, quota)
	if response["systemMessage"] != "Existing message\n"+postQuotaSystemMessage(quota) {
		t.Fatalf("existing message lost: %#v", response)
	}
}

func TestCodexHookPortQuotaDoesNotChangeReviewMode(t *testing.T) {
	for _, included := range []bool{false, true} {
		bundle := baseBundle("")
		bundle["claude_review_mode"] = "AUDIT"
		if included {
			bundle["codex_quota"] = map[string]any{"5h_used_pct": float64(24), "5h_resets_at": "2026-09-23T16:49:02Z"}
		}
		compact, _, err := finalizePostArtifacts(t.TempDir(), map[string]any{"session_id": "s", "tool_use_id": "u"}, bundle, 8000, 1200)
		mustPostTest(t, err)
		if compact["claude_review_mode"] != "AUDIT" || included && compact["codex_quota"] == nil {
			t.Fatalf("quota changed review mode: %#v", compact)
		}
	}
}

func TestCodexHookPortHookValidationPrecedence(t *testing.T) {
	for _, tc := range []struct {
		hook, worker, mode string
		sideEffects        bool
	}{
		{"PASS", "UNKNOWN", "AUDIT", false}, {"FAIL", "PASS", "FULL", false},
		{"UNKNOWN", "PASS", "AUDIT", false}, {"UNKNOWN", "UNKNOWN", "FULL", false},
		{"PASS", "PASS", "FULL", true}, {"", "UNKNOWN", "FULL", false},
	} {
		t.Run(tc.hook+"/"+tc.worker+fmt.Sprint(tc.sideEffects), func(t *testing.T) {
			bundle := baseBundle("")
			bundle["thread_id"], bundle["changed_paths"], bundle["delta"] = "thread", []string{"file.go"}, map[string]any{"status": "OK"}
			bundle["worker_validation"] = map[string]any{"status": tc.worker}
			bundle["reviewer1_skipped_reason"] = review.BelowThreshold
			if tc.hook != "" {
				bundle["hook_checks"] = map[string]any{"status": tc.hook, "results": []any{}}
			}
			pending := map[string]any{"task_id": "checks", "phase": "implementation", "route": "normal", "task_contract_grounded": true}
			finalizePostReview(bundle, pending, nil, review.GateDecision{Reason: review.BelowThreshold}, nil, true, false, tc.sideEffects, false)
			if bundle["claude_review_mode"] != tc.mode {
				t.Fatalf("validation precedence = %#v", bundle)
			}
		})
	}
}

func TestCodexHookPortPendingCheckSnapshot(t *testing.T) {
	repo := postDeltaRepo(t)
	changed := filepath.Join(repo, ".orchestrator", "profile.toml")
	mustPostTest(t, os.MkdirAll(filepath.Dir(changed), 0700))
	mustPostTest(t, os.WriteFile(changed, []byte("schema_version = 1\n[[checks]]\nname = \"changed\"\nargv = [\"missing-command\"]\n"), 0600))
	pending := map[string]any{"phase": "implementation", "route": "normal", "task_contract_grounded": true,
		"profile_checks": []any{map[string]any{"name": "original", "argv": []any{"git", "--version"}, "when": []any{"scope/**"}, "timeout_seconds": 10}}}
	checks := postConfiguredChecks(pending)
	if len(checks) != 1 || checks[0].Name != "original" {
		t.Fatalf("stored check snapshot = %#v", checks)
	}
	bundle := baseBundle("")
	bundle["delta"], bundle["thread_id"] = map[string]any{"status": "OK", "changed_paths": []string{"scope/file.txt"}, "changed_file_count": 1, "changed_line_count": 2}, "thread"
	schedulePostReview(repo, pending, bundle, postProfile{}, checks, map[string]any{"small_delta_max_files": 2, "small_delta_max_lines": 50}, time.Second, true, true)
	if mapValue(bundle["hook_checks"])["status"] != "PASS" {
		t.Fatalf("changed profile replaced pending checks: %#v", bundle)
	}
}

func TestCodexHookPortBundleSafetyAndRuntimeCaps(t *testing.T) {
	bundle := baseBundle("")
	paths, findings := []any{}, []any{}
	for i := 0; i < 4999; i++ {
		paths = append(paths, fmt.Sprintf("generated/path-%04d.txt", i))
	}
	paths = append(paths, "state/current.yaml")
	for i := 0; i < 20; i++ {
		findings = append(findings, map[string]any{"code": fmt.Sprintf("FINDING_%02d", i), "message": strings.Repeat("x", 2000)})
	}
	bundle["task_id"], bundle["thread_id"], bundle["phase"], bundle["routing_enforced"] = "stress", "thread", "implementation", true
	bundle["reviewer1_result"], bundle["validation"], bundle["concurrent_writer"] = "CHANGES_REQUIRED", "FAIL", true
	bundle["protected_paths_touched"], bundle["micro_repair"] = []any{"state/current.yaml"}, map[string]any{"attempted": false, "status": "SKIPPED_BUDGET"}
	bundle["changed_paths"], bundle["findings"], bundle["implementer_report"] = paths, findings, strings.Repeat("r", 5000)
	compact, path, err := finalizePostArtifacts(t.TempDir(), map[string]any{"session_id": "s", "tool_use_id": "stress"}, bundle, 8000, 1200)
	mustPostTest(t, err)
	size, err := nonReportBytes(compact)
	mustPostTest(t, err)
	if size > 8000 || compact["changed_path_count"] != 5000 || len(compact["changed_paths"].([]any)) > 10 || len(stringValue(compact["changed_paths_sha256"])) != 64 || compact["claude_review_mode"] != "FULL" || compact["reviewer1_result"] != "CHANGES_REQUIRED" || compact["validation"] != "FAIL" || compact["concurrent_writer"] != true || len(stringValue(compact["implementer_report"])) != 1200 || strings.Join(anyStringList(compact["protected_paths_touched"]), ",") != "state/current.yaml" || mapValue(compact["micro_repair"])["status"] != "SKIPPED_BUDGET" {
		t.Fatalf("safety fields or caps lost: %#v", compact)
	}
	for i, raw := range compact["findings"].([]any) {
		finding := mapValue(raw)
		if finding["code"] != fmt.Sprintf("FINDING_%02d", i) || finding["message_char_count"] != 2000 || len(stringValue(finding["message_sha256"])) != 64 {
			t.Fatalf("finding evidence lost: %#v", finding)
		}
	}
	full, err := readAsyncObject(path)
	mustPostTest(t, err)
	if len(full["changed_paths"].([]any)) != 5000 || len(stringValue(mapValue(full["findings"].([]any)[19])["message"])) != 2000 || len(stringValue(full["implementer_report"])) != 5000 {
		t.Fatal("full artifact lost unabridged evidence")
	}
	base := baseBundle("")
	base["phase"], base["reviewer1_skipped_reason"], base["implementer_report"] = "operation", "PHASE_OPERATION", strings.Repeat("operation deliverable ", 100)
	base["findings"] = []any{map[string]any{"code": "LONG_MESSAGE", "message": strings.Repeat("m", 6000)}}
	base["changed_paths"] = paths[:100]
	var lengths []int
	for _, tc := range []struct{ cap, report int }{{8192, 24000}, {4096, 37}} {
		compact, _, err := finalizePostArtifacts(t.TempDir(), map[string]any{"session_id": "s", "tool_use_id": "caps"}, base, tc.cap, postReportLimit(map[string]any{"operation_report_max_chars": tc.report}, "operation"))
		mustPostTest(t, err)
		size, err := nonReportBytes(compact)
		mustPostTest(t, err)
		if size > tc.cap || len(stringValue(compact["implementer_report"])) != min(tc.report, len(stringValue(base["implementer_report"]))) {
			t.Fatalf("runtime caps ignored: %#v", compact)
		}
		lengths = append(lengths, len(stringValue(mapValue(compact["findings"].([]any)[0])["message"])))
	}
	if lengths[1] >= lengths[0] {
		t.Fatalf("tighter cap did not shorten finding: %v", lengths)
	}
}

func TestCodexHookPortAdoptionOriginalBaselineAndRecovery(t *testing.T) {
	old := postReviewerBridge
	calls := 0
	postReviewerBridge = func(_ context.Context, _ string, _ map[string]any, _ time.Duration) (review.BridgeOutcome, error) {
		calls++
		return review.BridgeOutcome{Outcome: "completed", Attributes: map[string]any{"result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "validation": "PASS", "findings": []any{}}}, nil
	}
	t.Cleanup(func() { postReviewerBridge = old })
	for _, recovery := range []bool{false, true} {
		before := calls
		state, repo, event, index, pending, original, _, resolved, loaded := newAsyncSuccessCase(t, "success_delta_audit")
		project := projectKey(&repo)
		index["project_key"], pending["project_key"] = project, project
		mustPostTest(t, writeAsyncObject(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"), index))
		mustPostTest(t, writeAsyncObject(pendingPathForAsync(state, original), pending))
		if got := lease.New(filepath.Join(state, "projects", project)).Acquire(stringValue(pending["lease_owner"]), stringValue(pending["task_id"]), []string{"src"}, lease.AcquireOptions{}, float64(time.Now().Unix())); got.Status != "OK" {
			t.Fatal(got)
		}
		mustPostTest(t, os.WriteFile(filepath.Join(repo, "src", "file.go"), []byte("package src\n// changed\n"), 0600))
		var first map[string]any
		var err error
		if recovery {
			// Recreate the durable boundary after review but before consumption.
			first, err = finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil,
				map[string]any{"small_delta_max_files": 2, "small_delta_max_lines": 50}, 8000, 1200, time.Second)
			mustPostTest(t, err)
			index["stage"] = "FINALIZING"
			mustPostTest(t, writeAsyncObject(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"), index))
			mustPostTest(t, writeAsyncObject(pendingPathForAsync(state, original), pending))
		}
		event["session_id"], event["cwd"] = "new-session", repo
		_, _, _, _, ownershipErr := asyncOwned(state, event, repo)
		if ownership, ok := ownershipErr.(asyncOwnershipError); !ok || ownership.kind != "AsyncAdoptionRequired" {
			t.Fatalf("adoption did not require consent: %v", ownershipErr)
		}
		mapValue(event["tool_input"])["adopt"] = true
		_, adoptedIndex, adoptedPending, adoptedOriginal, err := asyncOwned(state, event, repo)
		mustPostTest(t, err)
		var response map[string]any
		if recovery {
			var handled bool
			response, handled, err = recoverAsyncFinalizing(state, adoptedOriginal, adoptedIndex, adoptedPending, 8000, 1200)
			mustPostTest(t, err)
			if !handled || !asyncSuccessEqual(response, first) {
				t.Fatal("adoption did not reuse the durable review")
			}
		} else {
			response, err = finishAsyncSuccess(state, event, adoptedIndex, adoptedPending, adoptedOriginal, repo, loaded, resolved, nil,
				map[string]any{"small_delta_max_files": 2, "small_delta_max_lines": 50}, 8000, 1200, time.Second)
			mustPostTest(t, err)
		}
		bundle := responseBundle(t, response)
		if bundle["task_id"] != pending["task_id"] || strings.Join(anyStringList(bundle["changed_paths"]), ",") != "src/file.go" || filepath.Base(stringValue(bundle["raw_output_path"])) != stringValue(index["call_key"])+".json" || isFile(pendingPathForAsync(state, original)) {
			t.Fatalf("adopted result lost original baseline/identity: %#v", bundle)
		}
		owner, err := lease.New(filepath.Join(state, "projects", project)).OwnerState(stringValue(pending["lease_owner"]), float64(time.Now().Unix()))
		mustPostTest(t, err)
		if owner != "RELEASED" {
			t.Fatalf("adopted lease = %s", owner)
		}
		lock, _, _, _, cached, err := claimAsyncResult(state, event, repo)
		mustPostTest(t, err)
		releaseTaskUsageLock(lock)
		if !asyncSuccessEqual(mapValue(cached), response) {
			t.Fatal("adopted repeat did not reuse consumed response")
		}
		if calls != before+1 {
			t.Fatalf("adoption reviewed %d times", calls-before)
		}
	}
}

func TestCodexHookPortAdoptionRejectsWrongProjectAndChain(t *testing.T) {
	state, repo, event, index, pending, original, _, _, _ := newAsyncSuccessCase(t, "success_no_delta")
	project := projectKey(&repo)
	index["project_key"], pending["project_key"] = project, project
	mustPostTest(t, writeAsyncObject(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"), index))
	mustPostTest(t, writeAsyncObject(pendingPathForAsync(state, original), pending))
	event["session_id"] = "new-session"
	mapValue(event["tool_input"])["adopt"] = true
	if _, _, _, _, err := asyncOwned(state, event, postDeltaRepo(t)); err == nil {
		t.Fatal("foreign project adoption admitted")
	}
	pending["job_id"] = strings.Repeat("0", 64)
	mustPostTest(t, writeAsyncObject(pendingPathForAsync(state, original), pending))
	if _, _, _, _, err := asyncOwned(state, event, repo); err == nil || err.Error() != "job and pending call do not match" {
		t.Fatalf("mismatched pending chain admitted: %v", err)
	}
}

func TestCodexHookPortAsyncReplyBindingAndLabels(t *testing.T) {
	for _, label := range []string{"new task", ""} {
		repo, state := codexHookPortWorkspace(t)
		seedPreThread(t, repo, state)
		call := codexHookPortCall(repo, "reply-label", "mcp__codex-main__codex_start", "Continue.")
		input := mapValue(call["tool_input"])
		input["mode"], input["threadId"] = "reply", "thread-p63"
		delete(input, "label")
		want := "saved label"
		if label != "" {
			input["label"], want = label, label
		}
		response, err := Pre(call, state)
		mustPostTest(t, err)
		updated := mapValue(mapValue(response["hookSpecificOutput"])["updatedInput"])
		pending, err := readAsyncObject(mustPendingPath(t, state, call))
		mustPostTest(t, err)
		if updated["mode"] != "reply" || updated["threadId"] != "thread-p63" || updated["model"] != nil || updated["config"] != nil || updated["jobLabel"] != want || updated["label"] != nil || pending["label"] != want || pending["prior_thread_id"] != "thread-p63" {
			t.Fatalf("reply binding/label = %#v pending=%#v", updated, pending)
		}
	}
}

func TestCodexHookPortScopeLeaseAdmission(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	call := func(id, scope, tool string) map[string]any {
		return codexHookPortCall(repo, id, tool, "ORCH|task="+id+"|phase=implementation|route=subtle|scope="+scope+"\nDo work.")
	}
	for _, event := range []map[string]any{call("owner-a", "scope/file.txt", "mcp__codex-main__codex"), call("owner-b", "outside/file.txt", "mcp__codex-main__codex_start")} {
		response, err := Pre(event, state)
		mustPostTest(t, err)
		if mapValue(mapValue(response["hookSpecificOutput"])["updatedInput"]) == nil {
			t.Fatalf("disjoint admission denied: %#v", response)
		}
	}
	for _, scope := range []string{"scope/file.txt", "scope"} {
		response, err := Pre(call("contender", scope, "mcp__codex-main__codex"), state)
		mustPostTest(t, err)
		reason := stringValue(mapValue(response["hookSpecificOutput"])["permissionDecisionReason"])
		if !strings.Contains(reason, "SCOPE_LEASE_CONFLICT") || !strings.Contains(reason, "owner-a") || !strings.Contains(reason, "leases --release") {
			t.Fatalf("conflict/prefix admission = %#v", response)
		}
	}
	data, err := os.ReadFile(filepath.Join(state, "telemetry", "events", "by_task", "contender.jsonl"))
	mustPostTest(t, err)
	if !strings.Contains(string(data), `"violation_code":"SCOPE_LEASE_CONFLICT"`) {
		t.Fatal("lease conflict protocol observation missing")
	}
}

func TestCodexHookPortMissingAsyncLeaseGrace(t *testing.T) {
	store := lease.New(filepath.Join(t.TempDir(), "projects", "project"))
	id := jobHash("missing-call")
	if got := store.Acquire("missing", "missing-task", []string{"file.go"}, lease.AcquireOptions{JobID: &id, TimeoutSeconds: 100}, 1000); got.Status != "OK" {
		t.Fatal(got)
	}
	if got := store.Acquire("next", "next-task", []string{"file.go"}, lease.AcquireOptions{}, 1599); got.Code != "SCOPE_LEASE_CONFLICT" {
		t.Fatal(got)
	}
	if owner, err := store.OwnerState("missing", 1600); err != nil || owner != "RELEASED" {
		t.Fatalf("missing job grace = %s %v", owner, err)
	}
	if got := store.Acquire("next", "next-task", []string{"file.go"}, lease.AcquireOptions{}, 1600); got.Status != "OK" {
		t.Fatal(got)
	}
}

func TestCodexHookPortQuotaGateWindowsAndOverride(t *testing.T) {
	phase, reason := "implementation", "new_task"
	parsed := policy.OrchParseResult{Valid: true, Phase: &phase, Reason: &reason}
	for _, tc := range []struct {
		window string
		used   float64
		deny   bool
	}{{"5h", 92, true}, {"7d", 95, true}, {"5h", 89, false}, {"", 0, false}} {
		quota := map[string]any(nil)
		if tc.window != "" {
			quota = map[string]any{tc.window + "_used_pct": tc.used, tc.window + "_resets_at": "2026-09-24T15:00:00Z"}
		}
		decision := decideFallbackPre(parsed, "session", nil, nil, quota, nil, time.Now())
		if tc.deny && (decision.Action != "deny" || decision.Code != "LOW_CODEX_QUOTA" || !strings.Contains(decision.Message, tc.window) || !strings.Contains(decision.Message, fmt.Sprintf("%g%%", tc.used))) || !tc.deny && decision.Action != "continue" {
			t.Fatalf("quota gate = %#v", decision)
		}
		if tc.window == "5h" && tc.deny {
			for _, want := range []string{"before this task starts", "reason=user_override", "background sleep command", "until the 5h reset at 2026-09-24T15:00:00Z UTC", "waiting costs nothing"} {
				if !strings.Contains(decision.Message, want) {
					t.Fatalf("quota denial lacks %q: %s", want, decision.Message)
				}
			}
		}
	}
	reason = "user_override"
	if got := decideFallbackPre(parsed, "session", nil, nil, map[string]any{"5h_used_pct": float64(92), "5h_resets_at": "2026-09-24T15:00:00Z"}, nil, time.Now()); got.Action != "continue" {
		t.Fatalf("no-fallback user override denied: %#v", got)
	}
}

func TestCodexHookPortAsyncStartResponseShapes(t *testing.T) {
	for _, shape := range []string{"string", "blocks", "mapping"} {
		repo, state := codexHookPortWorkspace(t)
		call := codexHookPortCall(repo, "start-shapes", "mcp__codex-main__codex_start", "ORCH|task=start-shapes|phase=implementation|route=subtle|scope=scope\nDo work.")
		pre, err := Pre(call, state)
		mustPostTest(t, err)
		id := jobHash(stringValue(mapValue(mapValue(pre["hookSpecificOutput"])["updatedInput"])["call"]))
		value := map[string]any{"jobId": id, "state": "RUNNING"}
		encoded, err := json.Marshal(value)
		mustPostTest(t, err)
		call["hook_event_name"], call["tool_response"] = "PostToolUse", map[string]any{"structuredContent": value}
		if shape == "string" {
			call["tool_response"] = string(encoded)
		} else if shape == "blocks" {
			call["tool_response"] = []any{map[string]any{"type": "text", "text": string(encoded)}}
		}
		response, err := PostToolUse(call, state)
		mustPostTest(t, err)
		bundle := responseBundle(t, response)
		pending, err := readAsyncObject(mustPendingPath(t, state, call))
		mustPostTest(t, err)
		index, err := readAsyncObject(filepath.Join(state, "async_jobs", id+".json"))
		mustPostTest(t, err)
		key, err := callKey(call)
		mustPostTest(t, err)
		path, err := ledger.PathForTask(filepath.Join(state, "projects", projectKey(&repo)), "start-shapes")
		mustPostTest(t, err)
		record := ledger.Load(path, "start-shapes")
		data, err := os.ReadFile(filepath.Join(state, "telemetry", "events", "by_task", "start-shapes.jsonl"))
		mustPostTest(t, err)
		if bundle["jobId"] != id || bundle["state"] != "RUNNING" || !strings.Contains(stringValue(bundle["next"]), id) || pending["job_id"] != id || index["call_key"] != key || record.Status != ledger.OK || record.Record["last_outcome"] != nil || record.Record["pending_call_baseline"] == nil || strings.Contains(string(data), `"event_type":"IMPLEMENTER_FINISHED"`) {
			t.Fatalf("%s start finalized prematurely: bundle=%#v pending=%#v record=%#v", shape, bundle, pending, record)
		}
	}
}

func TestCodexHookPortPluginToolNamesNewAndReply(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	call := codexHookPortCall(repo, "plugin-new", "mcp__plugin_purser_codex-main__codex", "ORCH|task=plugin-task|phase=implementation|route=subtle|scope=scope\n## Objective\nInspect scope.\n## Acceptance criteria\n- Report result.")
	pre, err := Pre(call, state)
	mustPostTest(t, err)
	if mapValue(mapValue(pre["hookSpecificOutput"])["updatedInput"])["model"] == nil {
		t.Fatalf("plugin new call was not routed: %#v", pre)
	}
	pending, err := readAsyncObject(mustPendingPath(t, state, call))
	mustPostTest(t, err)
	if pending["prior_thread_id"] != nil {
		t.Fatal("new plugin call recorded a prior thread")
	}
	call["hook_event_name"], call["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "plugin-thread", "content": "done"}}
	response, err := PostToolUse(call, state)
	mustPostTest(t, err)
	if responseBundle(t, response)["thread_id"] != "plugin-thread" {
		t.Fatal("plugin new result lost thread")
	}
	reply := codexHookPortCall(repo, "plugin-reply", "mcp__plugin_purser_codex-main__codex-reply", "Continue.")
	mapValue(reply["tool_input"])["threadId"] = "plugin-thread"
	pre, err = Pre(reply, state)
	mustPostTest(t, err)
	pending, err = readAsyncObject(mustPendingPath(t, state, reply))
	mustPostTest(t, err)
	if mapValue(mapValue(pre["hookSpecificOutput"])["updatedInput"])["threadId"] != "plugin-thread" || pending["prior_thread_id"] != "plugin-thread" {
		t.Fatalf("plugin reply binding lost: %#v", pre)
	}
	reply["hook_event_name"], reply["tool_response"] = "PostToolUse", call["tool_response"]
	response, err = PostToolUse(reply, state)
	mustPostTest(t, err)
	if responseBundle(t, response)["thread_id"] != "plugin-thread" {
		t.Fatal("plugin reply result lost thread")
	}
}

func TestCodexHookPortQuotaUTF8RoundTrip(t *testing.T) {
	response, err := replacement(baseBundle(""), "updatedToolOutput")
	mustPostTest(t, err)
	appendPostQuotaSystemMessage(response, map[string]any{"7d_used_pct": float64(46), "7d_resets_at": "2026-09-28T16:07:43Z"})
	encoded, err := canonicalJSON(response)
	mustPostTest(t, err)
	var decoded map[string]any
	mustPostTest(t, json.Unmarshal(encoded, &decoded))
	if !strings.Contains(string(encoded), "⚓") || decoded["systemMessage"] != response["systemMessage"] {
		t.Fatalf("quota Unicode damaged in JSON: %s", encoded)
	}
}

func TestCodexHookPortOverlappingWorktreeStarts(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	results := make(chan map[string]any, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			id := fmt.Sprintf("race-%d", i)
			response, err := Pre(codexHookPortCall(repo, id, "mcp__codex-main__codex_start", "ORCH|task="+id+"|phase=implementation|route=subtle|scope=scope\nDo work."), state)
			if err != nil {
				results <- map[string]any{"error": err.Error()}
				return
			}
			results <- response
		}(i)
	}
	for i := 0; i < 2; i++ {
		select {
		case response := <-results:
			updated := mapValue(mapValue(response["hookSpecificOutput"])["updatedInput"])
			if updated == nil || updated["worktree"] != true || updated["leaseQueued"] != nil {
				t.Fatalf("overlapping worktree start queued or denied: %#v", response)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("overlapping worktree starts did not complete")
		}
	}
	disjoint := codexHookPortCall(repo, "disjoint", "mcp__codex-main__codex_start", "ORCH|task=disjoint|phase=implementation|route=subtle|scope=outside\nDo work.")
	response, err := Pre(disjoint, state)
	mustPostTest(t, err)
	if updated := mapValue(mapValue(response["hookSpecificOutput"])["updatedInput"]); updated == nil || updated["leaseQueued"] != nil {
		t.Fatalf("disjoint worktree start queued: %#v", response)
	}
	value, err := readAsyncObject(filepath.Join(state, "projects", projectKey(&repo), "scope_leases.json"))
	mustPostTest(t, err)
	owners := mapValue(value["owners"])
	if len(owners) != 3 {
		t.Fatalf("active leases = %#v", owners)
	}
	for _, raw := range owners {
		if mapValue(raw)["state"] != "ACTIVE" {
			t.Fatalf("overlapping worktree lease is not active: %#v", raw)
		}
	}
}

func TestCodexHookPortQuotaBeforePending(t *testing.T) {
	for _, tc := range []struct {
		primary, secondary float64
		reason             string
		deny               bool
	}{
		{92, 0, "new_task", true}, {0, 95, "new_task", true}, {92, 0, "user_override", false},
		{89, 0, "new_task", false}, {-1, -1, "new_task", false},
	} {
		repo, state := codexHookPortWorkspace(t)
		if tc.primary >= 0 {
			now := time.Now().UTC()
			path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", now.Format("2006"), now.Format("01"), now.Format("02"), "rollout-quota.jsonl")
			snapshot := map[string]any{"timestamp": now.Format(time.RFC3339Nano), "payload": map[string]any{"rate_limits": map[string]any{
				"primary":   map[string]any{"window_minutes": 300, "used_percent": tc.primary, "resets_at": now.Add(time.Hour).Unix()},
				"secondary": map[string]any{"window_minutes": 10080, "used_percent": tc.secondary, "resets_at": now.Add(24 * time.Hour).Unix()},
			}}}
			mustPostTest(t, writeAsyncObject(path, snapshot))
		}
		call := codexHookPortCall(repo, "quota-pre", "mcp__codex-main__codex", "ORCH|task=quota-pre|phase=implementation|route=subtle|reason="+tc.reason+"|scope=scope\nDo work.")
		response, err := Pre(call, state)
		mustPostTest(t, err)
		denied := strings.Contains(stringValue(mapValue(response["hookSpecificOutput"])["permissionDecisionReason"]), "LOW_CODEX_QUOTA")
		if denied != tc.deny || isFile(mustPendingPath(t, state, call)) == tc.deny {
			t.Fatalf("quota ordering: response=%#v deny=%v", response, tc.deny)
		}
	}
}

func TestCodexHookPortSyncFinalizationReleasesLease(t *testing.T) {
	for _, failure := range []bool{false, true} {
		repo, state := codexHookPortWorkspace(t)
		call := codexHookPortCall(repo, "first", "mcp__codex-main__codex", "ORCH|task=first|phase=implementation|route=subtle|scope=scope\nDo work.")
		contender := codexHookPortCall(repo, "next", "mcp__codex-main__codex", "ORCH|task=next|phase=implementation|route=subtle|scope=scope\nDo work.")
		_, err := Pre(call, state)
		mustPostTest(t, err)
		denial, err := Pre(contender, state)
		mustPostTest(t, err)
		if mapValue(denial["hookSpecificOutput"])["permissionDecision"] != "deny" {
			t.Fatal("contender admitted before finalization")
		}
		if failure {
			call["hook_event_name"], call["error"] = "PostToolUseFailure", "worker failed"
			PostToolUseFailure(call, state)
		} else {
			call["hook_event_name"], call["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "lease-thread", "content": "done"}}
			_, err := PostToolUse(call, state)
			mustPostTest(t, err)
		}
		key, err := callKey(call)
		mustPostTest(t, err)
		owner, err := lease.New(filepath.Join(state, "projects", projectKey(&repo))).OwnerState(key, float64(time.Now().Unix()))
		mustPostTest(t, err)
		admitted, err := Pre(contender, state)
		mustPostTest(t, err)
		if owner != "RELEASED" || mapValue(mapValue(admitted["hookSpecificOutput"])["updatedInput"]) == nil {
			t.Fatalf("lease was not released: owner=%s next=%#v", owner, admitted)
		}
	}
}

func TestCodexHookPortStalePrefixLeaseBlocksAdmission(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	store := lease.New(filepath.Join(state, "projects", projectKey(&repo)))
	if got := store.Acquire("lost-owner", "old-task", []string{"scope"}, lease.AcquireOptions{}, float64(time.Now().Unix())); got.Status != "OK" {
		t.Fatal(got)
	}
	call := codexHookPortCall(repo, "new-task", "mcp__codex-main__codex", "ORCH|task=new-task|phase=implementation|route=subtle|scope=scope/file.txt\nDo work.")
	denied, err := Pre(call, state)
	mustPostTest(t, err)
	reason := stringValue(mapValue(denied["hookSpecificOutput"])["permissionDecisionReason"])
	if !strings.Contains(reason, "SCOPE_LEASE_CONFLICT") || !strings.Contains(reason, "old-task") {
		t.Fatalf("owner without pending evidence did not block prefix: %#v", denied)
	}
	if got := store.Release("lost-owner", float64(time.Now().Unix())); got.Status != "OK" {
		t.Fatal(got)
	}
	admitted, err := Pre(call, state)
	mustPostTest(t, err)
	if mapValue(mapValue(admitted["hookSpecificOutput"])["updatedInput"]) == nil {
		t.Fatalf("manual release did not admit contender: %#v", admitted)
	}
	wide := codexHookPortCall(repo, "broad", "mcp__codex-main__codex", "ORCH|task=broad|phase=implementation|route=subtle|scope=scope\nDo work.")
	denied, err = Pre(wide, state)
	mustPostTest(t, err)
	if !strings.Contains(stringValue(mapValue(denied["hookSpecificOutput"])["permissionDecisionReason"]), "SCOPE_LEASE_CONFLICT") {
		t.Fatal("broad scope did not conflict with narrow owner")
	}
}

func TestCodexHookPortReleasedLeaseBeforeStartDoesNotExcusePath(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	store := lease.New(filepath.Join(state, "projects", projectKey(&repo)))
	if got := store.Acquire("former", "former-task", []string{"outside"}, lease.AcquireOptions{}, float64(time.Now().Unix()-1)); got.Status != "OK" {
		t.Fatal(got)
	}
	if got := store.Release("former", float64(time.Now().Unix()-1)); got.Status != "OK" {
		t.Fatal(got)
	}
	call := codexHookPortCall(repo, "owned", "mcp__codex-main__codex", "ORCH|task=owned|phase=implementation|route=subtle|scope=scope\nDo work.")
	_, err := Pre(call, state)
	mustPostTest(t, err)
	mustPostTest(t, os.WriteFile(filepath.Join(repo, "outside", "file.txt"), []byte("foreign edit\n"), 0600))
	call["hook_event_name"], call["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "owned-thread", "content": "done"}}
	response, err := PostToolUse(call, state)
	mustPostTest(t, err)
	bundle := responseBundle(t, response)
	if bundle["claude_review_mode"] != "FULL" || bundle["known_concurrent_scopes"] != nil {
		t.Fatalf("released foreign owner excused later edit: %#v", bundle)
	}
}

func TestCodexHookPortCheckFailureAndUnknownEvidence(t *testing.T) {
	for _, status := range []string{"FAIL", "UNKNOWN"} {
		repo := postDeltaRepo(t)
		argv := []any{"git", "invalid-port-command"}
		if status == "UNKNOWN" {
			argv = []any{filepath.Join(repo, "missing-command")}
		}
		pending := map[string]any{"phase": "implementation", "route": "normal", "task_contract_grounded": true,
			"profile_checks": []any{map[string]any{"name": "focused", "argv": argv, "when": []any{"scope/**"}, "timeout_seconds": 10}}}
		bundle := baseBundle("")
		bundle["thread_id"], bundle["worker_validation"], bundle["delta"] = "thread", map[string]any{"status": "PASS"}, map[string]any{"status": "OK", "changed_paths": []string{"scope/file.txt"}, "changed_file_count": 1, "changed_line_count": 2}
		schedulePostReview(repo, pending, bundle, postProfile{}, postConfiguredChecks(pending), map[string]any{"small_delta_max_files": 2, "small_delta_max_lines": 50}, 2*time.Second, true, true)
		checks := mapValue(bundle["hook_checks"])
		if checks["status"] != status {
			t.Fatalf("check outcome = %#v", checks)
		}
		if _, leaked := mapValue(checks["results"].([]any)[0])["tail"]; leaked {
			t.Fatal("check output tail leaked into compact check results")
		}
		code := "HOOK_CHECK_" + map[string]string{"FAIL": "FAILED", "UNKNOWN": "UNKNOWN"}[status]
		found := false
		for _, raw := range bundle["findings"].([]any) {
			finding := mapValue(raw)
			if finding["code"] == code && strings.Contains(stringValue(finding["message"]), "focused:") && stringValue(finding["message"]) != "focused: " {
				found = true
			}
		}
		if !found || status == "FAIL" && bundle["claude_review_mode"] != "FULL" || status == "UNKNOWN" && bundle["claude_review_mode"] != "AUDIT" {
			t.Fatalf("check evidence or worker fallback lost: %#v", bundle)
		}
	}
}

func TestCodexHookPortTwoAsyncJobsFinalizeIndependently(t *testing.T) {
	state, repo, _, _, basePending, _, _, resolved, loaded := newAsyncSuccessCase(t, "success_no_delta")
	type startedCall struct {
		id                              string
		event, index, pending, original map[string]any
	}
	var started []startedCall
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("parallel-%d", i)
		original := map[string]any{"session_id": "parallel-session", "tool_use_id": id, "tool_name": "mcp__codex-main__codex"}
		key, err := callKey(original)
		mustPostTest(t, err)
		call, jobID := "call-"+id, jobHash("call-"+id)
		pending := map[string]any{}
		for k, v := range basePending {
			pending[k] = v
		}
		pending["task_id"], pending["session_id"], pending["tool_use_id"] = id, original["session_id"], id
		pending["job_id"], pending["async_call"], pending["lease_owner"] = jobID, call, key
		index := map[string]any{"jobId": jobID, "call": call, "call_key": key, "project_key": pending["project_key"], "session_id": original["session_id"], "stage": "FINALIZING"}
		job := map[string]any{"jobId": jobID, "call": call, "state": "SUCCEEDED", "threadId": "thread-" + id, "repoRoot": repo}
		mustPostTest(t, writeAsyncObject(filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), job))
		mustPostTest(t, writeAsyncObject(pendingPathForAsync(state, original), pending))
		mustPostTest(t, writeAsyncObject(filepath.Join(state, "async_jobs", jobID+".json"), index))
		if got := lease.New(filepath.Join(state, "projects", stringValue(pending["project_key"]))).Acquire(key, id, []string{fmt.Sprintf("file%d", i)}, lease.AcquireOptions{}, float64(time.Now().Unix())); got.Status != "OK" {
			t.Fatal(got)
		}
		event := map[string]any{"session_id": original["session_id"], "tool_use_id": "result-" + id, "tool_name": "mcp__codex-main__codex_result", "tool_input": map[string]any{"jobId": jobID}, "tool_response": map[string]any{"structuredContent": map[string]any{"jobId": jobID, "state": "SUCCEEDED", "threadId": "thread-" + id, "content": "done"}}}
		started = append(started, startedCall{id, event, index, pending, original})
	}
	for _, call := range started {
		id, event, index, pending, original := call.id, call.event, call.index, call.pending, call.original
		response, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil, nil, 8000, 1200, time.Second)
		mustPostTest(t, err)
		if responseBundle(t, response)["task_id"] != id {
			t.Fatal("async result used another task's pending state")
		}
		lock, _, _, _, cached, err := claimAsyncResult(state, event, repo)
		mustPostTest(t, err)
		releaseTaskUsageLock(lock)
		if !asyncSuccessEqual(mapValue(cached), response) {
			t.Fatal("async task did not retain its own cached result")
		}
	}
}

func TestCodexHookPortReplyLeaseAttributionAndConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		repo, state := codexHookPortWorkspace(t)
		first := codexHookPortCall(repo, "repair-first", "mcp__codex-main__codex", "ORCH|task=repair|phase=implementation|route=subtle|scope=scope\nDo work.")
		foreign := codexHookPortCall(repo, "foreign", "mcp__codex-main__codex", "ORCH|task=foreign|phase=implementation|route=subtle|scope=outside\nDo work.")
		_, err := Pre(foreign, state)
		mustPostTest(t, err)
		_, err = Pre(first, state)
		mustPostTest(t, err)
		mustPostTest(t, os.WriteFile(filepath.Join(repo, "scope", "file.txt"), []byte("first repair\n"), 0600))
		first["hook_event_name"], first["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "repair-thread", "content": "done"}}
		_, err = PostToolUse(first, state)
		mustPostTest(t, err)
		if conflict {
			other := codexHookPortCall(repo, "other", "mcp__codex-main__codex", "ORCH|task=other|phase=implementation|route=subtle|scope=scope\nDo work.")
			_, err = Pre(other, state)
			mustPostTest(t, err)
		}
		reply := codexHookPortCall(repo, "repair-reply", "mcp__codex-main__codex_start", "Continue the repair.")
		mapValue(reply["tool_input"])["mode"], mapValue(reply["tool_input"])["threadId"] = "reply", "repair-thread"
		response, err := Pre(reply, state)
		mustPostTest(t, err)
		key, err := callKey(reply)
		mustPostTest(t, err)
		leasePath := filepath.Join(state, "projects", projectKey(&repo), "scope_leases.json")
		value, err := readAsyncObject(leasePath)
		mustPostTest(t, err)
		owner := mapValue(mapValue(value["owners"])[key])
		if conflict {
			if !strings.Contains(stringValue(mapValue(response["hookSpecificOutput"])["permissionDecisionReason"]), "SCOPE_LEASE_CONFLICT") || owner != nil {
				t.Fatalf("conflicting reply admitted: %#v owner=%#v", response, owner)
			}
			continue
		}
		if owner["state"] != "ACTIVE" || owner["task_id"] != "repair" || strings.Join(anyStringList(owner["scope"]), ",") != "scope" {
			t.Fatalf("reply did not inherit the repair lease: %#v", owner)
		}
		call := stringValue(mapValue(mapValue(response["hookSpecificOutput"])["updatedInput"])["call"])
		jobID := jobHash(call)
		reply["hook_event_name"], reply["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"jobId": jobID, "state": "RUNNING"}}
		_, err = PostToolUse(reply, state)
		mustPostTest(t, err)
		mustPostTest(t, os.WriteFile(filepath.Join(repo, "scope", "file.txt"), []byte("second repair\n"), 0600))
		foreign["hook_event_name"], foreign["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "foreign-thread", "content": "done"}}
		response, err = PostToolUse(foreign, state)
		mustPostTest(t, err)
		bundle := responseBundle(t, response)
		artifact, err := readAsyncObject(stringValue(bundle["full_bundle_path"]))
		mustPostTest(t, err)
		known := artifact["known_concurrent_scopes"].([]any)
		if len(anyStringList(bundle["changed_paths"])) != 0 || len(known) != 1 || mapValue(known[0])["task_id"] != "repair" || mapValue(known[0])["path"] != "scope/file.txt" {
			t.Fatalf("reply repair attributed to foreign result: %#v full=%#v", bundle, artifact)
		}
		store := lease.New(filepath.Dir(leasePath))
		if owner, err := store.OwnerState(key, float64(time.Now().Unix())); err != nil || owner != "ACTIVE" {
			t.Fatalf("reply lease lost before its finalization: %s %v", owner, err)
		}
		mustPostTest(t, writeAsyncObject(filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), map[string]any{"jobId": jobID, "call": call, "state": "SUCCEEDED", "threadId": "repair-thread", "repoRoot": repo}))
		result := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "mcp__codex-main__codex_result", "session_id": "port-session", "tool_use_id": "reply-result", "tool_input": map[string]any{"jobId": jobID}, "tool_response": map[string]any{"structuredContent": map[string]any{"jobId": jobID, "state": "SUCCEEDED", "threadId": "repair-thread", "content": "done"}}}
		_, err = PostToolUse(result, state)
		mustPostTest(t, err)
		if owner, err := store.OwnerState(key, float64(time.Now().Unix())); err != nil || owner != "RELEASED" {
			t.Fatalf("reply lease was not released: %s %v", owner, err)
		}
	}
}

func TestCodexHookPortForeignEditsForceFull(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	call := codexHookPortCall(repo, "owned", "mcp__codex-main__codex", "ORCH|task=owned|phase=implementation|route=subtle|scope=scope\nDo work.")
	_, err := Pre(call, state)
	mustPostTest(t, err)
	mustPostTest(t, os.WriteFile(filepath.Join(repo, "outside", "file.txt"), []byte("foreign edit\n"), 0600))
	call["hook_event_name"], call["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "owned-thread", "content": "done"}}
	response, err := PostToolUse(call, state)
	mustPostTest(t, err)
	bundle := responseBundle(t, response)
	found := false
	for _, raw := range bundle["findings"].([]any) {
		if mapValue(raw)["code"] == "OUT_OF_SCOPE_MODIFICATION" {
			found = true
		}
	}
	if bundle["claude_review_mode"] != "FULL" || bundle["known_concurrent_scopes"] != nil || !found {
		t.Fatalf("unleased foreign edit was excused: %#v", bundle)
	}
}

func TestCodexHookPortAsyncStartRequiresOrch(t *testing.T) {
	repo, state := codexHookPortWorkspace(t)
	call := codexHookPortCall(repo, "missing-orch", "mcp__codex-main__codex_start", "Do work.")
	response, err := Pre(call, state)
	mustPostTest(t, err)
	if mapValue(response["hookSpecificOutput"])["permissionDecision"] != "deny" {
		t.Fatalf("async call without ORCH was admitted: %#v", response)
	}
	if _, err := os.Stat(pendingPathForAsync(state, call)); !os.IsNotExist(err) {
		t.Fatalf("denied async call created pending state: %v", err)
	}
}

func TestCodexHookPortQuotaNewReplyBundleAndReceipt(t *testing.T) {
	for _, withQuota := range []bool{false, true} {
		repo, state := codexHookPortWorkspace(t)
		if withQuota {
			now := time.Now()
			path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", now.Format("2006"), now.Format("01"), now.Format("02"), "rollout-port-quota-thread.jsonl")
			mustPostTest(t, writeAsyncObject(path, map[string]any{"timestamp": now.UTC().Format(time.RFC3339), "payload": map[string]any{"rate_limits": map[string]any{"primary": map[string]any{"window_minutes": 300, "used_percent": 24, "resets_at": now.Add(time.Hour).Unix()}}}}))
		}
		for _, tool := range []string{"mcp__codex-main__codex", "mcp__codex-main__codex-reply"} {
			call := codexHookPortCall(repo, tool, tool, "ORCH|task=quota|phase=implementation|route=subtle|scope=scope\nInspect.")
			if strings.HasSuffix(tool, "reply") {
				mapValue(call["tool_input"])["threadId"], mapValue(call["tool_input"])["prompt"] = "quota-thread", "Continue."
			}
			_, err := Pre(call, state)
			mustPostTest(t, err)
			call["hook_event_name"], call["tool_response"] = "PostToolUse", map[string]any{"structuredContent": map[string]any{"threadId": "quota-thread", "content": "done"}}
			response, err := PostToolUse(call, state)
			mustPostTest(t, err)
			bundle := responseBundle(t, response)
			quota := mapValue(bundle["codex_quota"])
			receipt := mapValue(readPostReceiptState(postReceiptStatePath(state, repo), "port-session")["receipt_pending"])
			if bundle["claude_review_mode"] != "FULL" || receipt["model"] != bundle["model"] || withQuota && (quota["5h_used_pct"] != float64(24) || receipt["5h_used_pct"] != float64(24)) || !withQuota && (quota != nil || receipt["5h_used_pct"] != nil) {
				t.Fatalf("quota bundle/receipt mismatch: bundle=%#v receipt=%#v", bundle, receipt)
			}
		}
	}
}

func TestCodexHookPortReleasedLeasePruning(t *testing.T) {
	root := t.TempDir()
	now := float64(time.Now().Unix())
	path := filepath.Join(root, "scope_leases.json")
	mustPostTest(t, writeAsyncObject(path, map[string]any{"version": 1, "owners": map[string]any{
		"expired": map[string]any{"state": "RELEASED", "scope": []any{"old"}, "task_id": "old", "released_at": now - 8*86400},
		"recent":  map[string]any{"state": "RELEASED", "scope": []any{"recent"}, "task_id": "recent", "released_at": now - 10},
	}}))
	if got := lease.New(root).Acquire("new", "new", []string{"new"}, lease.AcquireOptions{}, now); got.Status != "OK" {
		t.Fatal(got)
	}
	value, err := readAsyncObject(path)
	mustPostTest(t, err)
	owners := mapValue(value["owners"])
	if owners["expired"] != nil || owners["recent"] == nil {
		t.Fatalf("released lease retention changed: %#v", owners)
	}
}
