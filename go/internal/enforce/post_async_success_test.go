package enforce

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

func TestFinishAsyncSuccessKeepsStringMapFindings(t *testing.T) {
	state, repo, event, index, pending, original, _, resolved, loaded := newAsyncSuccessCase(t, "success_delta_audit")
	if err := os.WriteFile(filepath.Join(repo, "src", "file.go"), []byte("package src\n// worker delta\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved.ReviewerCandidates = []profile.ReviewerCandidate{{Agent: "agy", Model: "gemini-3.8-flash-high"}}
	previousBridge := postReviewerBridge
	postReviewerBridge = func(context.Context, string, map[string]any, time.Duration) (review.BridgeOutcome, error) {
		return review.BridgeOutcome{Outcome: "completed", Attributes: map[string]any{
			"result": "CHANGES_REQUIRED", "review_status": "COMPLETED", "validation": "FAIL",
			"findings": []map[string]string{{"code": "ASYNC_REVIEW_FINDING", "message": "Keep this async finding."}},
		}}, nil
	}
	t.Cleanup(func() { postReviewerBridge = previousBridge })
	response, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil,
		map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	attempt, attemptState, attemptErr := LoadTaskAttempt(state, pendingProjectKey(pending), stringValue(pending["task_id"]))
	if attemptErr != nil || attemptState != TaskAttemptOK || attempt.TerminalDisposition != "CHANGES_REQUIRED" {
		t.Fatalf("reviewed task outcome = %#v state=%s err=%v", attempt, attemptState, attemptErr)
	}
	findings := responseBundle(t, response)["findings"].([]any)
	for _, raw := range findings {
		finding := mapValue(raw)
		if finding["code"] == "ASYNC_REVIEW_FINDING" && finding["message"] == "Keep this async finding." {
			return
		}
	}
	t.Fatalf("async finding missing from bundle: %#v", findings)
}

func TestPostAsyncDiscardedCancelReturnsOnlyReceipt(t *testing.T) {
	state, repo, event, index, _, _, job, _, _ := newAsyncSuccessCase(t, "discarded_cancel")
	event["tool_name"] = "mcp__codex-main__codex_cancel"
	event["cwd"] = repo
	job["discarded"] = true
	if err := atomicAdmissionJSON(filepath.Join(state, "codex_exec_mcp", "jobs", stringValue(index["jobId"])+".json"), job); err != nil {
		t.Fatal(err)
	}
	response, err := postAsyncDispatch(event, state)
	if err != nil {
		t.Fatal(err)
	}
	if got := responseBundle(t, response); !reflect.DeepEqual(got, map[string]any{
		"jobId": index["jobId"], "state": "SUCCEEDED", "discarded": true,
	}) {
		t.Fatalf("discard receipt = %#v", got)
	}
	if _, exists := response["systemMessage"]; exists {
		t.Fatalf("discard receipt added completion guidance: %#v", response)
	}
	var saved map[string]any
	data, err := os.ReadFile(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"))
	if err != nil || json.Unmarshal(data, &saved) != nil {
		t.Fatal(err)
	}
	if saved["stage"] != "CONSUMED" {
		t.Fatalf("discard receipt was not cached: %#v", saved)
	}
}

func TestDurableReviewFinalizationPythonOracleReplay(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "durable_review_finalization.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string                    `json:"oracle"`
		Cases  map[string]map[string]any `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "codex._async_post_followup durable async review finalization" {
		t.Fatalf("oracle fixture invalid: %v %#v", err, fixture)
	}
	for _, name := range []string{"normal", "finalizing_recovery", "reviewer_bridge_unavailable"} {
		t.Run(name, func(t *testing.T) {
			state, repo, event, index, pending, original, _, resolved, _ := newAsyncSuccessCase(t, "success_delta_audit")
			if err := os.WriteFile(filepath.Join(repo, "src", "file.go"), []byte("package src\n// review delta\n"), 0600); err != nil {
				t.Fatal(err)
			}
			loaded := map[string]any{"post_tool_output_field": "updatedToolOutput", "reviewer1_model_requested": "gemini-3.8-flash-high"}
			gate := map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}
			callsPath := filepath.Join(state, "reviewer.calls")
			if name == "reviewer_bridge_unavailable" {
				configureNativeReviewer(t, state, &resolved, "", "")
			} else {
				configureNativeReviewer(t, state, &resolved, "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "PASS")
			}
			t.Setenv("PURser_ASYNC_REVIEW_CALLS", callsPath)
			indexPath := filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json")
			if err := writeAsyncObject(indexPath, index); err != nil {
				t.Fatal(err)
			}
			response, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil, gate, 8000, 1200, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			bundle := responseBundle(t, response)
			want := fixture.Cases[name]
			if bundle["claude_review_mode"] != want["mode"] || bundle["reviewer1_result"] != want["reviewer_result"] || index["stage"] != want["stage"] {
				t.Fatalf("oracle mismatch bundle=%#v stage=%v want=%#v", bundle, index["stage"], want)
			}
			if expected, ok := want["reviewer_calls"].(float64); ok {
				data, readErr := os.ReadFile(callsPath)
				if readErr != nil || float64(strings.Count(string(data), "called")) != expected {
					t.Fatalf("review calls=%q want %v err=%v", data, expected, readErr)
				}
			}
			if name == "finalizing_recovery" {
				index["stage"] = "FINALIZING"
				if err := writeAsyncObject(indexPath, index); err != nil {
					t.Fatal(err)
				}
				recovered, handled, recoverErr := recoverAsyncFinalizing(state, original, index, pending, 8000, 1200)
				data, readErr := os.ReadFile(callsPath)
				if recoverErr != nil || !handled || !asyncSuccessEqual(recovered, response) || readErr != nil || strings.Count(string(data), "called") != 1 {
					t.Fatalf("retry response mismatch handled=%v calls=%q err=%v read=%v", handled, data, recoverErr, readErr)
				}
				lock, _, _, _, cached, claimErr := claimAsyncResult(state, event, repo)
				if claimErr != nil || !asyncSuccessEqual(mapValue(cached), recovered) {
					t.Fatalf("second result did not reuse consumed response: cached=%#v err=%v", cached, claimErr)
				}
				releaseTaskUsageLock(lock)
			}
			if name == "reviewer_bridge_unavailable" && (bundle["claude_review_mode"] != "FULL" || bundle["reviewer1_result"] != nil) {
				t.Fatalf("unavailable reviewer was represented as passing: %#v", bundle)
			}
			if want["bundle_persisted"] == true && !isFile(filepath.Join(state, "bundle_artifacts", stringValue(index["call_key"])+".json")) {
				t.Fatal("bundle artifact was not persisted")
			}
		})
	}
}

func TestPostAsyncSuccessRemainingFixtureCases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_async_success.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases map[string]map[string]any `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"success_no_delta", "success_delta_audit", "success_reviewer_full", "finalizing_recovery", "unapplied_worktree_lease_retained"} {
		t.Run(name, func(t *testing.T) {
			state, repo, event, index, pending, original, job, resolved, loaded := newAsyncSuccessCase(t, name)
			retirementCalls := 0
			priorDispatcher := WorktreeRetirementDispatcher
			WorktreeRetirementDispatcher = func(string, string) error {
				retirementCalls++
				return errors.New("launcher unavailable")
			}
			t.Cleanup(func() { WorktreeRetirementDispatcher = priorDispatcher })
			if name == "success_delta_audit" || name == "success_reviewer_full" {
				if err := os.WriteFile(filepath.Join(repo, "src", "file.go"), []byte("package src\n// worker delta\n"), 0600); err != nil {
					t.Fatal(err)
				}
				result := "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"
				validation := "PASS"
				if name == "success_reviewer_full" {
					result, validation = "UNCERTAIN", "UNKNOWN"
				}
				callsPath := filepath.Join(state, "reviewer.calls")
				configureNativeReviewer(t, state, &resolved, result, validation)
				t.Setenv("PURser_ASYNC_REVIEW_CALLS", callsPath)
			}
			response, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil,
				map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, 5*time.Second)
			if name == "finalizing_recovery" {
				if err != nil {
					t.Fatal(err)
				}
				index["stage"] = "FINALIZING"
				if err = writeAsyncObject(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"), index); err != nil {
					t.Fatal(err)
				}
				recovered, handled, recoverErr := recoverAsyncFinalizing(state, original, index, pending, 8000, 1200)
				if recoverErr != nil || !handled || !asyncSuccessEqual(recovered, response) {
					t.Fatalf("recovery equal=%v first=%#v recovered=%#v handled=%v err=%v", asyncSuccessEqual(recovered, response), responseBundle(t, response), responseBundle(t, recovered), handled, recoverErr)
				}
				response = recovered
			} else if err != nil {
				t.Fatal(err)
			}
			wantRetirementCalls := 1
			if name == "success_delta_audit" || name == "success_reviewer_full" {
				wantRetirementCalls = 0
			}
			if retirementCalls != wantRetirementCalls {
				t.Fatalf("retirement calls=%d want %d", retirementCalls, wantRetirementCalls)
			}
			if wantRetirementCalls > 0 {
				diagnostic, readErr := os.ReadFile(filepath.Join(state, "diagnostics.jsonl"))
				if readErr != nil || !strings.Contains(string(diagnostic), "launcher unavailable") {
					t.Fatalf("retirement dispatch diagnostic=%q err=%v", diagnostic, readErr)
				}
			}
			bundle := responseBundle(t, response)
			want := fixture.Cases[name]
			wantMode := want["mode"]
			if name == "success_no_delta" {
				wantMode = want["response_state"]
			}
			if wantMode != nil && bundle["claude_review_mode"] != wantMode {
				t.Fatalf("mode=%v want %v", bundle["claude_review_mode"], wantMode)
			}
			if bundle["reviewer1_result"] != want["reviewer_result"] && want["reviewer_result"] != nil {
				t.Fatalf("reviewer result=%v want %v", bundle["reviewer1_result"], want["reviewer_result"])
			}
			if calls, ok := want["reviewer_calls"].(float64); ok {
				data, readErr := os.ReadFile(filepath.Join(state, "reviewer.calls"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if got := float64(strings.Count(string(data), "called")); got != calls {
					t.Fatalf("reviewer calls=%v want %v", got, calls)
				}
			}
			stored, readErr := readAsyncObject(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"))
			if readErr != nil || stored["stage"] != want["stage"] || !reflect.DeepEqual(stored["response"], response) {
				t.Fatalf("consumed state=%#v err=%v", stored, readErr)
			}
			if want["cached_repeat_equal"] == true {
				lock, _, _, _, cached, claimErr := claimAsyncResult(state, event, repo)
				if claimErr != nil || !reflect.DeepEqual(cached, response) {
					t.Fatalf("cached response mismatch: %v", claimErr)
				}
				releaseTaskUsageLock(lock)
			}
			artifact, readErr := readAsyncObject(filepath.Join(state, "bundle_artifacts", stringValue(index["call_key"])+".json"))
			if readErr != nil || !asyncSuccessSubset(artifact, bundle) {
				t.Fatalf("bundle artifact mismatch: %v", readErr)
			}
			pendingPath := pendingPathForAsync(state, original)
			_, pendingErr := os.Stat(pendingPath)
			if want["pending_retained"] == true && pendingErr != nil {
				t.Fatalf("pending missing: %v", pendingErr)
			}
			if want["pending_retained"] != true && !os.IsNotExist(pendingErr) {
				t.Fatalf("pending cleanup: %v", pendingErr)
			}
			if retained, ok := want["lease_retention_after"].(float64); ok {
				leaseState, leaseErr := readAsyncObject(filepath.Join(state, "projects", stringValue(index["project_key"]), "scope_leases.json"))
				owner := mapValue(mapValue(leaseState["owners"])[stringValue(pending["lease_owner"])])
				if name == "unapplied_worktree_lease_retained" {
					if leaseErr != nil || owner["state"] != "RELEASED" {
						t.Fatalf("no-delta worktree lease state=%v err=%v", owner["state"], leaseErr)
					}
				} else if leaseErr != nil || owner["retention_after"] != retained {
					t.Fatalf("lease retention=%v want %v err=%v", owner["retention_after"], retained, leaseErr)
				}
			}
			_ = job
		})
	}
}

func TestFinalizeNoDeltaReleasesScopeLease(t *testing.T) {
	state, repo, event, index, pending, original, _, resolved, loaded := newAsyncSuccessCase(t, "unapplied_worktree_lease_retained")
	if _, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil,
		map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	leases, err := readAsyncObject(filepath.Join(state, "projects", stringValue(pending["project_key"]), "scope_leases.json"))
	owner := mapValue(mapValue(leases["owners"])[stringValue(pending["lease_owner"])])
	if err != nil || owner["state"] != "RELEASED" {
		t.Fatalf("no-delta lease state=%v err=%v", owner["state"], err)
	}
}

func TestAsyncSuccessValidationUsesRecordedTestEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, wantWorker, wantBundle, wantSource string
		evidence                                 []any
		evidenceFailed                           bool
	}{
		{name: "pass", wantWorker: "PASS", wantBundle: "PASS", wantSource: "HOST_TEST_EVIDENCE", evidence: []any{map[string]any{"command": "go test ./...", "exit_code": float64(0)}}},
		{name: "fail", wantWorker: "FAIL", wantBundle: "FAIL", wantSource: "HOST_TEST_EVIDENCE", evidence: []any{map[string]any{"command": "go test ./...", "exit_code": float64(1)}}},
		{name: "capped_out_failure", wantWorker: "FAIL", wantBundle: "FAIL", wantSource: "HOST_TEST_EVIDENCE", evidence: []any{
			map[string]any{"command": "go test ./pkg0", "exit_code": float64(0)},
			map[string]any{"command": "go test ./pkg1", "exit_code": float64(0)},
			map[string]any{"command": "go test ./pkg2", "exit_code": float64(0)},
			map[string]any{"command": "go test ./pkg3", "exit_code": float64(0)},
			map[string]any{"command": "go test ./pkg4", "exit_code": float64(0)},
			map[string]any{"command": "go test ./pkg5", "exit_code": float64(0)},
		}, evidenceFailed: true},
		{name: "malformed", wantWorker: "UNKNOWN", wantBundle: "UNKNOWN", wantSource: "HOST_TEST_EVIDENCE", evidence: []any{map[string]any{"command": "go test ./..."}}},
		{name: "latest_same_command_passes", wantWorker: "PASS", wantBundle: "PASS", wantSource: "HOST_TEST_EVIDENCE", evidence: []any{
			map[string]any{"command": "go test ./...", "exit_code": float64(1)},
			map[string]any{"command": "go test ./...", "exit_code": float64(0)},
		}},
		{name: "latest_same_command_fails", wantWorker: "FAIL", wantBundle: "FAIL", wantSource: "HOST_TEST_EVIDENCE", evidence: []any{
			map[string]any{"command": "command A", "exit_code": float64(0)},
			map[string]any{"command": "command A", "exit_code": float64(1)},
		}},
		{name: "worker_fail_overridden", wantWorker: "PASS", wantBundle: "PASS", wantSource: "HOST_TEST_EVIDENCE", evidence: []any{map[string]any{"command": "go test ./...", "exit_code": float64(0)}}},
		{name: "no_evidence_fallback", wantWorker: "PASS", wantBundle: "UNKNOWN", wantSource: "WORKER_REPORTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, repo, event, index, pending, original, job, resolved, loaded := newAsyncSuccessCase(t, tc.name)
			pending["worker_validation"] = map[string]any{"source": "WORKER_REPORTED", "status": "PASS", "summary": "passed"}
			if tc.name == "worker_fail_overridden" {
				pending["worker_validation"] = map[string]any{"source": "WORKER_REPORTED", "status": "FAIL", "summary": "failed"}
			}
			if tc.evidence != nil {
				job["testEvidence"] = tc.evidence
				if tc.evidenceFailed {
					job["testEvidenceFailed"] = true
				}
				if err := atomicAdmissionJSON(filepath.Join(state, "codex_exec_mcp", "jobs", stringValue(index["jobId"])+".json"), job); err != nil {
					t.Fatal(err)
				}
			}
			response, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil,
				map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			bundle := responseBundle(t, response)
			worker := mapValue(bundle["worker_validation"])
			if worker["status"] != tc.wantWorker || bundle["validation"] != tc.wantBundle {
				t.Fatalf("worker validation=%v bundle validation=%v want worker=%s bundle=%s", worker, bundle["validation"], tc.wantWorker, tc.wantBundle)
			}
			if worker["source"] != tc.wantSource {
				t.Fatalf("worker source=%v want %s", worker["source"], tc.wantSource)
			}
			if tc.evidence == nil {
				if _, exists := bundle["test_evidence"]; exists {
					t.Fatalf("unexpected test_evidence: %#v", bundle["test_evidence"])
				}
				if _, exists := worker["test_evidence"]; exists {
					t.Fatalf("unexpected worker test_evidence: %#v", worker["test_evidence"])
				}
			} else if !reflect.DeepEqual(worker["test_evidence"], tc.evidence) {
				t.Fatalf("worker test_evidence=%#v want %#v", worker["test_evidence"], tc.evidence)
			}
		})
	}
}

func TestPostAsyncPlanCompletionStoresCompleteArtifact(t *testing.T) {
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("USERPROFILE", t.TempDir())
	state, _, event, _, pending, original, _, _, _ := newAsyncSuccessCase(t, "plan_completion")
	pending["phase"] = "plan"
	plan := "# Summary: Async plan\n\nThe complete plan is stored here.\n\n## Milestone\nImplement it.\n"
	event["tool_response"] = map[string]any{"structuredContent": map[string]any{
		"jobId": stringValue(event["tool_input"].(map[string]any)["jobId"]), "state": "SUCCEEDED", "threadId": "thread-fixture", "content": plan,
	}}
	if err = writeAsyncObject(pendingPathForAsync(state, original), pending); err != nil {
		t.Fatal(err)
	}
	response, err := PostToolUse(event, state)
	if err != nil {
		t.Fatal(err)
	}
	bundle := responseBundle(t, response)
	planPath := stringValue(bundle["plan_path"])
	if planPath == "" || stringValue(bundle["plan_summary"]) == "" {
		t.Fatalf("async plan artifact pointer missing: %#v", bundle)
	}
	content, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != plan {
		t.Fatalf("stored plan=%q want %q", content, plan)
	}
	replayed, err := PostToolUse(event, state)
	if err != nil {
		t.Fatal(err)
	}
	if got := stringValue(responseBundle(t, replayed)["plan_path"]); got != planPath {
		t.Fatalf("replayed plan path=%q want %q", got, planPath)
	}
}

func TestReplyAfterAppliedWorktreeFinalizesOnlyReplyDelta(t *testing.T) {
	state, repo, event, index, pending, original, job, resolved, loaded := newAsyncSuccessCase(t, "applied_reply")
	worktree := filepath.Join(state, "reply-worktree")
	runAsyncSuccessGit(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "src", "file.go"), []byte("package src\n// applied\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := ledger.RebaseAppliedReply(repo, worktree)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "src", "file.go"))
	if err != nil || !strings.Contains(string(content), "// applied") {
		t.Fatalf("reply worktree did not include applied state: %q err=%v", content, err)
	}
	if err = os.WriteFile(filepath.Join(worktree, "src", "reply.go"), []byte("package src\n// reply\n"), 0600); err != nil {
		t.Fatal(err)
	}
	job["worktreePath"], job["repoRoot"], job["snapshot"], job["applyState"] = worktree, repo, snapshot, "applied"
	job["finishedAt"] = float64(1700000000)
	// An applied worktree must be diffed against its applied baseline directly;
	// replay state from the earlier application cannot block reply finalization.
	prep := filepath.Join(state, "preparations", stringValue(index["jobId"]))
	if err = os.MkdirAll(prep, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(prep, "state.json"), []byte(`{"state":"CONFLICT","conflicts":["src/file.go"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = atomicAdmissionJSON(filepath.Join(state, "codex_exec_mcp", "jobs", stringValue(index["jobId"])+".json"), job); err != nil {
		t.Fatal(err)
	}
	response, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil,
		map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	bundle := responseBundle(t, response)
	if !reflect.DeepEqual(bundle["changed_paths"], []any{"src/reply.go"}) {
		t.Fatalf("reply delta includes applied changes or omits reply changes: bundle=%#v", bundle)
	}
}

func TestUnfinalizableAsyncResultIsTerminal(t *testing.T) {
	root := t.TempDir()
	id := jobHash("unfinalizable async result")
	index := map[string]any{"jobId": id, "stage": "FINALIZING"}
	response, err := failAsyncFinalization(root, index, nil, "updatedToolOutput", errors.New("finalize failed"))
	if err != nil || response == nil {
		t.Fatalf("failure finalization response=%#v err=%v", response, err)
	}
	stored, err := readAsyncObject(filepath.Join(root, "async_jobs", id+".json"))
	if err != nil || stored["stage"] != "FAILED" {
		t.Fatalf("async result did not reach terminal FAILED state: %#v err=%v", stored, err)
	}
}

func TestNonRetryableAsyncFinalizationReleasesAndPromotesLease(t *testing.T) {
	root := t.TempDir()
	project, queued := "project-finalize", "queued-owner"
	owner := jobHash("terminal finalize owner")
	store := lease.New(filepath.Join(root, "projects", project))
	if got := store.Acquire(owner, "failed-task", []string{"README.md"}, lease.AcquireOptions{}, 100); got.Status != "OK" {
		t.Fatalf("seed owner: %#v", got)
	}
	queuedJob := jobHash("queued job")
	if got := store.Acquire(queued, "queued-task", []string{"README.md"}, lease.AcquireOptions{JobID: &queuedJob, QueueConflict: true}, 101); got.Code != "LEASE_QUEUED" {
		t.Fatalf("seed queued owner: %#v", got)
	}
	call := "terminal finalize failure"
	id := jobHash(call)
	index := map[string]any{"jobId": id, "call": call, "call_key": owner, "project_key": project,
		"session_id": "failure-session", "stage": "FINALIZING"}
	pending := map[string]any{"lease_owner": owner, "project_key": project}
	response, err := failAsyncFinalization(root, index, pending, "updatedToolOutput", errors.New("bundle write failed"))
	if err != nil || response == nil {
		t.Fatalf("failure finalization response=%#v err=%v", response, err)
	}
	stored, err := readAsyncObject(filepath.Join(root, "async_jobs", id+".json"))
	if err != nil || stored["stage"] != "FAILED" {
		t.Fatalf("async result did not reach terminal FAILED state: %#v err=%v", stored, err)
	}
	leases, err := readAsyncObject(filepath.Join(root, "projects", project, "scope_leases.json"))
	owners := mapValue(leases["owners"])
	if err != nil || mapValue(owners[owner])["state"] != "RELEASED" || mapValue(owners[queued])["state"] != "ACTIVE" {
		t.Fatalf("terminal cleanup did not promote queued owner: %#v err=%v", owners, err)
	}
	lock, claimed, _, _, cached, err := claimAsyncResult(root, map[string]any{"session_id": "failure-session",
		"tool_name": "mcp__codex-main__codex_result", "tool_input": map[string]any{"jobId": id}}, "")
	if err != nil || claimed["stage"] != "FAILED" || !reflect.DeepEqual(cached, response) {
		t.Fatalf("cached terminal response=%#v index=%#v err=%v", cached, claimed, err)
	}
	releaseTaskUsageLock(lock)
}

func TestReplayPreparationFailureIsRetryableAndCanSucceed(t *testing.T) {
	state, repo, event, index, pending, original, job, resolved, loaded := newAsyncSuccessCase(t, "retry_replay_prepare")
	worktree := repo
	head, headErr := testutil.Hide(exec.Command("git", "-C", repo, "rev-parse", "HEAD")).Output()
	if headErr != nil {
		t.Fatal(headErr)
	}
	snapshot := strings.TrimSpace(string(head))
	job["worktreePath"], job["repoRoot"], job["snapshot"], job["snapshotRevision"] = worktree, repo, snapshot, 1
	recovery := filepath.Join(state, "preparations", stringValue(index["jobId"]), "revision-1")
	if err := os.MkdirAll(filepath.Join(recovery, "worker-tree"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recovery, "state.json"), []byte(`{"state":"PREPARING","snapshot":"`+snapshot+`","target":"`+snapshot+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recovery, "worker.patch"), []byte("not a git patch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeAsyncObject(filepath.Join(state, "codex_exec_mcp", "jobs", stringValue(index["jobId"])+".json"), job); err != nil {
		t.Fatal(err)
	}
	_, err := finishAsyncSuccess(state, event, index, pending, original, repo, loaded, resolved, nil,
		map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "status=error") {
		t.Fatalf("replay preparation failure lost cause: %v", err)
	}
	preparing, readErr := readAsyncObject(filepath.Join(recovery, "state.json"))
	if readErr != nil || preparing["state"] != "PREPARING" {
		t.Fatalf("PREPARING recovery state not retained: %#v, %v", preparing, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(recovery, "worker.patch")); statErr != nil {
		t.Fatalf("worker patch not retained: %v", statErr)
	}
	response, err := retryAsyncFinalization(state, index, err, stringValue(loaded["post_tool_output_field"]))
	if err != nil || response == nil || index["stage"] != "RETRYABLE" {
		t.Fatalf("replay preparation was not left retryable: stage=%v response=%#v err=%v", index["stage"], response, err)
	}
	leases, err := readAsyncObject(filepath.Join(state, "projects", stringValue(index["project_key"]), "scope_leases.json"))
	owner := mapValue(mapValue(leases["owners"])[stringValue(pending["lease_owner"])])
	if err != nil || owner["state"] != "ACTIVE" || !strings.Contains(stringValue(mapValue(mapValue(response["hookSpecificOutput"])["updatedToolOutput"].([]any)[0])["text"]), "status=error") {
		t.Fatalf("retryable response/lease lost cause or owner: response=%#v owner=%#v err=%v", response, owner, err)
	}
	if err = os.WriteFile(filepath.Join(recovery, "worker.patch"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	event["tool_input"] = map[string]any{"jobId": index["jobId"]}
	if err = writeAsyncObject(filepath.Join(state, "async_jobs", stringValue(index["jobId"])+".json"), index); err != nil {
		t.Fatal(err)
	}
	lock, retryIndex, retryPending, retryOriginal, _, claimErr := claimAsyncResult(state, event, repo)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	defer func() { releaseTaskUsageLock(lock); _ = lock.Close() }()
	_, err = finishAsyncSuccess(state, event, retryIndex, retryPending, retryOriginal, repo, loaded, resolved, nil,
		map[string]any{"small_delta_max_files": 0, "small_delta_max_lines": 0}, 8000, 1200, 5*time.Second)
	if err != nil {
		t.Fatalf("later result did not resume successfully: %v", err)
	}
}

func asyncSuccessEqual(a, b map[string]any) bool {
	left, leftErr := canonicalJSON(a)
	right, rightErr := canonicalJSON(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func asyncSuccessSubset(full, compact map[string]any) bool {
	for key, value := range compact {
		if key == "changed_paths_sha256" || key == "changed_path_count" || key == "delta" ||
			key == "force_full_review" || key == "implementer_report" {
			continue // response-only derived fields differ from the durable Python bundle.
		}
		if !reflect.DeepEqual(full[key], value) {
			return false
		}
	}
	return true
}

func newAsyncSuccessCase(t *testing.T, name string) (state, repo string, event, index, pending, original, job map[string]any, resolved postProfile, loaded map[string]any) {
	t.Helper()
	state = t.TempDir()
	repo = filepath.Join(state, "repo")
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
	data, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &baseline); err != nil {
		t.Fatal(err)
	}
	project, owner, task, call := "project-fixture", "lease-fixture", "task-fixture", "fixture call"
	jobID := jobHash(call)
	original = map[string]any{"session_id": "session", "tool_use_id": "origin", "tool_name": "mcp__codex-main__codex"}
	key, err := callKey(original)
	if err != nil {
		t.Fatal(err)
	}
	event = map[string]any{"session_id": "session", "tool_use_id": "result", "tool_name": "mcp__codex-main__codex_result", "hook_event_name": "PostToolUse", "tool_input": map[string]any{"jobId": jobID}, "tool_response": map[string]any{"structuredContent": map[string]any{"jobId": jobID, "state": "SUCCEEDED", "threadId": "thread-fixture", "content": "done"}}}
	index = map[string]any{"jobId": jobID, "call": call, "call_key": key, "project_key": project, "session_id": "session", "stage": "FINALIZING"}
	pending = map[string]any{"task_id": task, "phase": "implementation", "scope": []any{"src"}, "baseline": baseline, "lease_owner": owner, "project_key": project, "session_id": "session", "route": "normal", "model": "openai/gpt-5", "effort": "medium", "agent": "agy", "task_contract_grounded": true, "worker_validation": map[string]any{"status": "PASS", "summary": "passed"}, "task_objective": "review async result", "acceptance_criteria": "record the outcome", "tool_use_id": "origin", "job_id": jobID, "async_call": call, "repo_root": repo}
	job = map[string]any{"jobId": jobID, "call": call, "state": "SUCCEEDED", "threadId": "thread-fixture", "repoRoot": repo}
	if name == "unapplied_worktree_lease_retained" {
		head, err := testutil.Hide(exec.Command("git", "-C", repo, "rev-parse", "HEAD")).Output()
		if err != nil {
			t.Fatal(err)
		}
		job["worktreePath"], job["snapshot"], job["finishedAt"] = repo, strings.TrimSpace(string(head)), float64(1700000000)
	}
	if err = atomicAdmissionJSON(filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), job); err != nil {
		t.Fatal(err)
	}
	if result := lease.New(filepath.Join(state, "projects", project)).Acquire(owner, task, []string{"src"}, lease.AcquireOptions{}, float64(time.Now().Unix())); result.Status != "OK" {
		t.Fatalf("lease: %#v", result)
	}
	if err = os.MkdirAll(filepath.Join(state, "async_jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeAsyncObject(filepath.Join(state, "async_jobs", jobID+".json"), index); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(state, "pending"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeAsyncObject(pendingPathForAsync(state, original), pending); err != nil {
		t.Fatal(err)
	}
	return state, repo, event, index, pending, original, job, postProfile{Stored: true, ProtectedPaths: []string{}, SkipMaxFiles: intAddr(0), SkipMaxLines: intAddr(0), ReviewerCandidates: []profile.ReviewerCandidate{{Agent: "agy", Model: "gemini-3.8-flash-high"}}}, map[string]any{"post_tool_output_field": "updatedToolOutput"}
}

func runAsyncSuccessGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := testutil.Hide(exec.Command("git", append([]string{"-C", repo}, args...)...))
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

func configureNativeReviewer(t *testing.T, state string, resolved *postProfile, result, validation string) {
	t.Helper()
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(state, "plugin")
	configDir := filepath.Join(plugin, ".claude", "hooks", "orchestrator")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"reviewer1_model_requested":  "gemini-3.8-flash-high",
		"reviewer1_timeout_seconds":  60,
		"reviewer1_packet_max_bytes": 200000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	source := `package main
import ("fmt"; "os")
func main() { if calls := os.Getenv("PURser_ASYNC_REVIEW_CALLS"); calls != "" { f, err := os.OpenFile(calls, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); if err == nil { _, _ = f.WriteString("called\n"); _ = f.Close() } }; fmt.Print(os.Getenv("PURser_ASYNC_REVIEW_OUTPUT")) }
`
	sourcePath := filepath.Join(state, "mock_reviewer.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	name := "mock-reviewer"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(state, name)
	if output, err := testutil.Hide(exec.Command(goExecutable, "build", "-o", executable, sourcePath)).CombinedOutput(); err != nil {
		t.Fatalf("build native reviewer: %v: %s", err, output)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	resolved.ReviewerExecutable = executable
	resolved.ReviewerCandidates = []profile.ReviewerCandidate{{Agent: "fixture-test", Model: "fixture-model"}}
	registerAsyncNativeReviewer(t, executable, result == "")
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	if result == "" {
		return
	}
	envelope, err := json.Marshal(map[string]any{
		"conversation_id":  "fixture-review",
		"status":           "success",
		"response":         "",
		"duration_seconds": 0.01,
		"num_turns":        1,
		"usage":            map[string]any{},
		"structured_output": map[string]any{
			"result": result, "validation": validation, "findings": []any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURser_ASYNC_REVIEW_OUTPUT", string(envelope))
}

type asyncNativeReviewer struct {
	executable string
	panicBuild bool
}

func (asyncNativeReviewer) Provider() string        { return "fixture-test" }
func (asyncNativeReviewer) AllowedModel() string    { return "" }
func (asyncNativeReviewer) Family(string) string    { return "" }
func (asyncNativeReviewer) ValidateModel(any) error { return nil }
func (a asyncNativeReviewer) BuildCommand(string, map[string]any, map[string]any, review.CommandOptions) ([]string, error) {
	if a.panicBuild {
		panic("fixture reviewer panic")
	}
	return []string{a.executable}, nil
}
func (asyncNativeReviewer) Environment([]string) map[string]string {
	return map[string]string{"PATH": ""}
}
func (asyncNativeReviewer) LaunchOptions() review.ProcessOptions        { return review.ProcessOptions{} }
func (asyncNativeReviewer) ConfinementConfig([]string) (string, []byte) { return "", nil }
func (asyncNativeReviewer) ParseEnvelope(stdout string) (map[string]any, error) {
	parsed, err := review.ParseEnvelope("agy", []byte(stdout), true)
	if err != nil {
		return nil, err
	}
	return parsed.Map(), nil
}

func registerAsyncNativeReviewer(t *testing.T, executable string, panicBuild bool) {
	t.Helper()
	agy, err := review.ReviewerAdapter("agy")
	if err != nil {
		t.Fatal(err)
	}
	if err := review.RegisterReviewerAdapter("fixture-test", asyncNativeReviewer{executable: executable, panicBuild: panicBuild}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = review.RegisterReviewerAdapter("fixture-test", agy) })
	postReviewerAdapters["fixture-test"] = true
	t.Cleanup(func() { delete(postReviewerAdapters, "fixture-test") })
}
