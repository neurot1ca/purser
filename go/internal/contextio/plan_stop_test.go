package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopDoesNotBlockCompletionReceiptAndPreservesOtherDecisions(t *testing.T) {
	root, repo, _ := planFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "done"}})
	path := filepath.Join(root, "projects", repositoryProjectKey(repo), "telemetry", "compaction_advisor.json")
	state := readAdvisorState(path, "plan-stop-session")
	state["receipt_pending"] = map[string]any{"model": "model", "effort": "high", "5h_used_pct": 24, "7d_used_pct": 61, "snapshot_at": time.Now().Add(-2 * time.Hour).Format(time.RFC3339), "7d_resets_at": time.Now().Add(74 * time.Hour).Format(time.RFC3339), "blocked": false}
	if err := writeAdvisorState(path, state); err != nil {
		t.Fatal(err)
	}
	if got := planStop(t, repo, transcript, false); got != "" {
		t.Fatalf("stale/weekly receipt blocked Stop: %s", got)
	}
	state = readAdvisorState(path, "plan-stop-session")
	state["version_pending"] = map[string]any{"version": "2", "installed_version": "1", "delivered": true, "blocked": false}
	if err := writeAdvisorState(path, state); err != nil {
		t.Fatal(err)
	}
	if got := planStop(t, repo, transcript, false); got == "" || strings.Contains(got, "Codex done") {
		t.Fatalf("unrelated Stop decision lost or receipt added: %s", got)
	}
}

func TestPlanStopContinuationLifecycle(t *testing.T) {
	root, repo, artifact := planFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	taskID := "plan-1-m1of2-p1c"
	if _, matched, err := BindPlanJob(root, repo, taskID, "job-1"); err != nil || !matched {
		t.Fatalf("bind pass: matched=%v err=%v", matched, err)
	}
	if err := RecordPlanApply(root, repo, taskID, "job-1"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlanForTask(root, repo, state.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "What is the weather?"}})
	first := planStop(t, repo, transcript, false)
	wantReason := "Continue accepted plans plan-1 M1/2 dispatch through normal Purser routing and review."
	if !strings.Contains(first, wantReason) {
		t.Fatalf("first Stop reason = %q, want to contain %q", first, wantReason)
	}
	if duplicate := planStop(t, repo, transcript, false); duplicate != "" {
		t.Fatalf("duplicate Stop blocked same user turn: %q", duplicate)
	}
	if nested := planStop(t, repo, transcript, true); nested != "" {
		t.Fatalf("nested Stop blocked: %q", nested)
	}

	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Another unrelated question."}})
	if next := planStop(t, repo, transcript, false); !strings.Contains(next, "plan-1 M1/2") {
		t.Fatalf("next milestone Stop reason = %q", next)
	}

	state, err = UpdatePlan(root, repo, "plan-1", state.Revision, "stop", "")
	if err != nil {
		t.Fatal(err)
	}
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Please pause the plan."}})
	if stopped := planStop(t, repo, transcript, false); stopped != "" {
		t.Fatalf("explicitly stopped plan blocked: %q", stopped)
	}
	state, err = UpdatePlan(root, repo, "plan-1", state.Revision, "resume", "")
	if err != nil {
		t.Fatal(err)
	}
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue."}})
	if resumed := planStop(t, repo, transcript, false); !strings.Contains(resumed, "plan-1 M1/2") {
		t.Fatalf("resumed plan Stop reason = %q", resumed)
	}
}

func TestPlanStopFailureCasesFailOpen(t *testing.T) {
	for _, test := range []struct {
		name string
		fail func(*testing.T, string, string, *PlanState)
	}{
		{name: "gate", fail: func(t *testing.T, root, repo string, state *PlanState) {
			t.Helper()
			if _, err := UpdatePlan(root, repo, state.TaskID, state.Revision, "gate", "approval required"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "internal error", fail: func(t *testing.T, root, repo string, state *PlanState) {
			t.Helper()
			path := filepath.Join(root, "projects", state.ProjectKey, "active_plan.json")
			if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, repo, artifact := planFixture(t)
			t.Setenv("PURSER_STATE_DIR", root)
			state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
			if err != nil {
				t.Fatal(err)
			}
			test.fail(t, root, repo, state)
			transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
			appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Unrelated request."}})
			if got := planStop(t, repo, transcript, false); got != "" {
				t.Fatalf("Stop blocked on %s: %q", test.name, got)
			}
		})
	}

	t.Run("dispatch already in turn", func(t *testing.T) {
		root, repo, artifact := planFixture(t)
		t.Setenv("PURSER_STATE_DIR", root)
		if _, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1}); err != nil {
			t.Fatal(err)
		}
		transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
		appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue."}})
		appendPlanStopRecord(t, transcript, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "mcp__plugin_purser_codex-main__codex_start", "input": map[string]any{"prompt": "ORCH|task=plan-1-m1of1-p1c|phase=implement"}}}}})
		if got := planStop(t, repo, transcript, false); got != "" {
			t.Fatalf("Stop blocked after same-turn plan dispatch: %q", got)
		}
	})

	t.Run("linked job still running", func(t *testing.T) {
		root, repo, artifact := planFixture(t)
		t.Setenv("PURSER_STATE_DIR", root)
		if _, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1}); err != nil {
			t.Fatal(err)
		}
		jobID, taskID := strings.Repeat("a", 64), "plan-1-m1of1-p1c"
		if _, matched, err := BindPlanJob(root, repo, taskID, jobID); err != nil || !matched {
			t.Fatalf("bind running job: matched=%v err=%v", matched, err)
		}
		jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
		if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
			t.Fatal(err)
		}
		job, _ := json.Marshal(map[string]any{"jobId": jobID, "taskId": taskID, "state": "RUNNING"})
		if err := os.WriteFile(jobPath, job, 0600); err != nil {
			t.Fatal(err)
		}
		transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
		appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Unrelated request."}})
		if got := planStop(t, repo, transcript, false); got != "" {
			t.Fatalf("Stop blocked while linked job is running: %q", got)
		}
	})

	t.Run("unlinked running reply job still running", func(t *testing.T) {
		root, repo, artifact := planFixture(t)
		t.Setenv("PURSER_STATE_DIR", root)
		if _, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1}); err != nil {
			t.Fatal(err)
		}
		jobID, taskID := strings.Repeat("b", 64), "plan-1-m1of1-p1c"
		jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
		if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
			t.Fatal(err)
		}
		job, _ := json.Marshal(map[string]any{"jobId": jobID, "taskId": taskID, "state": "RUNNING", "mode": "reply"})
		if err := os.WriteFile(jobPath, job, 0600); err != nil {
			t.Fatal(err)
		}
		transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
		appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Unrelated request."}})
		if got := planStop(t, repo, transcript, false); got != "" {
			t.Fatalf("Stop blocked while an unlinked reply job is running: %q", got)
		}
	})
}

func TestPlanStopEvaluatesPlansIndependently(t *testing.T) {
	root, repo, artifact := planFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	planA, err := AcceptPlan(root, repo, "plan-a", artifact, []int{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	planB, err := AcceptPlan(root, repo, "plan-b", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	jobID, taskID := strings.Repeat("c", 64), "plan-a-m1of5-p1c"
	if _, matched, err := BindPlanJob(root, repo, taskID, jobID); err != nil || !matched {
		t.Fatalf("bind plan A: matched=%v err=%v", matched, err)
	}
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
	if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{"jobId": jobID, "taskId": taskID, "state": "RUNNING"})
	if err := os.WriteFile(jobPath, job, 0600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue."}})
	got := planStop(t, repo, transcript, false)
	if !strings.Contains(got, "plan-b M1/2") || strings.Contains(got, "plan-a") || strings.Contains(got, "plan: ") {
		t.Fatalf("Stop continuation did not isolate plan B or retained full pointer: %q", got)
	}
	if duplicate := planStop(t, repo, transcript, false); duplicate != "" {
		t.Fatalf("repeated Stop added duplicate text: %q", duplicate)
	}
	if _, err := UpdatePlan(root, repo, planB.TaskID, planB.Revision, "gate", "approval"); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdatePlan(root, repo, planA.TaskID, planA.Revision+1, "stop", ""); err != nil {
		t.Fatal(err)
	}
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Another turn."}})
	if stopped := planStop(t, repo, transcript, false); stopped != "" {
		t.Fatalf("stopped/gated siblings should not become eligible: %q", stopped)
	}
}

func TestPlanStopRemindsApplyAndDispatchPlans(t *testing.T) {
	root, repo, artifact := planFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	_, err := AcceptPlan(root, repo, "plan-apply", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "plan-dispatch", artifact, []int{1}); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue both plans."}})
	got := planStop(t, repo, transcript, false)
	if !strings.Contains(got, "plan-apply M1/2 dispatch") || !strings.Contains(got, "plan-dispatch M1/1 dispatch") {
		t.Fatalf("Stop omitted a plan's next action: %q", got)
	}
}

func TestPlanStopResumesAfterResolvedLinkedJobs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{
		{name: "hand applied", fields: map[string]any{"state": "SUCCEEDED", "applyState": "applied", "applyReason": "hand_applied_detected"}},
		{name: "discarded", fields: map[string]any{"state": "SUCCEEDED", "discarded": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repo, artifact := planFixture(t)
			t.Setenv("PURSER_STATE_DIR", root)
			_, err := AcceptPlan(root, repo, "plan-resolved", artifact, []int{1, 2})
			if err != nil {
				t.Fatal(err)
			}
			jobID, taskID := strings.Repeat("d", 64), "plan-resolved-m1of2-p1c"
			if _, matched, err := BindPlanJob(root, repo, taskID, jobID); err != nil || !matched {
				t.Fatalf("bind plan job: matched=%v err=%v", matched, err)
			}
			job := map[string]any{"jobId": jobID, "taskId": taskID}
			for key, value := range tc.fields {
				job[key] = value
			}
			jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
			if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(job)
			if err := os.WriteFile(jobPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			plans, err := ReconcilePlans(root, repo)
			if err != nil || len(plans) != 1 || plans[0].PendingNextAction != "dispatch" {
				t.Fatalf("reconciled plan = %#v, err=%v; want dispatch", plans, err)
			}
			transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
			appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue the plan."}})
			if got := planStop(t, repo, transcript, false); !strings.Contains(got, "plan-resolved M1/2 dispatch") {
				t.Fatalf("Stop did not resume the next pass: %q", got)
			}
		})
	}
}

func TestPlanStopKeepsApplyForUnappliedSucceededJob(t *testing.T) {
	root, repo, artifact := planFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	if _, err := AcceptPlan(root, repo, "plan-unapplied", artifact, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	jobID, taskID := strings.Repeat("e", 64), "plan-unapplied-m1of2-p1c"
	if _, matched, err := BindPlanJob(root, repo, taskID, jobID); err != nil || !matched {
		t.Fatalf("bind plan job: matched=%v err=%v", matched, err)
	}
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
	if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"jobId": jobID, "taskId": taskID, "state": "SUCCEEDED"})
	if err := os.WriteFile(jobPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	plans, err := ReconcilePlans(root, repo)
	if err != nil || len(plans) != 1 || plans[0].PendingNextAction != "apply" {
		t.Fatalf("reconciled plan = %#v, err=%v; want apply", plans, err)
	}
}

func TestPlanStopCompactSizeDoesNotExceedLegacySingleton(t *testing.T) {
	root, repo, artifact := planFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	plan, err := AcceptPlan(root, repo, "plan-single", artifact, []int{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue."}})
	got := planStop(t, repo, transcript, false)
	legacy := "Continue accepted plan " + plan.TaskID + " at milestone 1 of 5: dispatch the next pass through normal Purser routing and review. " + renderActivePlanPointer(repo)
	t.Logf("singleton Stop characters=%d; legacy singleton characters=%d", len(got), len(legacy))
	if len(got) > len(legacy) {
		t.Fatalf("singleton Stop grew from %d to %d characters", len(legacy), len(got))
	}
	second, err := AcceptPlan(root, repo, "plan-second", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	_ = second
	secondTranscript := filepath.Join(t.TempDir(), "two-plans.jsonl")
	appendPlanStopRecord(t, secondTranscript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue both."}})
	twoPlan := planStop(t, repo, secondTranscript, false)
	t.Logf("two-plan Stop characters=%d", len(twoPlan))
	if !strings.Contains(twoPlan, "plan-second M1/2") || strings.Contains(twoPlan, "plan-single") {
		t.Fatalf("new sibling was hidden by the same-turn reminder: %q", twoPlan)
	}
}

func TestPlanStopIneligibleSiblingDoesNotSuppress(t *testing.T) {
	for _, status := range []string{"stopped", "gated", "completed", "dispatched"} {
		t.Run(status, func(t *testing.T) {
			root, repo, artifact := planFixture(t)
			t.Setenv("PURSER_STATE_DIR", root)
			a, err := AcceptPlan(root, repo, "plan-a", artifact, []int{1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptPlan(root, repo, "plan-b", artifact, []int{1}); err != nil {
				t.Fatal(err)
			}
			switch status {
			case "stopped":
				_, err = UpdatePlan(root, repo, a.TaskID, a.Revision, "stop", "")
			case "gated":
				_, err = UpdatePlan(root, repo, a.TaskID, a.Revision, "gate", "approval")
			case "completed":
				_, err = UpdatePlan(root, repo, a.TaskID, a.Revision, "complete-milestone", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
			appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Continue."}})
			if status == "dispatched" {
				appendPlanStopRecord(t, transcript, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "mcp__plugin_purser_codex-main__codex_start", "input": map[string]any{"prompt": "ORCH|task=plan-a-m1of1-p1c|phase=implement"}}}}})
			}
			got := planStop(t, repo, transcript, false)
			if !strings.Contains(got, "plan-b M1/1") || strings.Contains(got, "plan-a") {
				t.Fatalf("%s plan A suppressed eligible plan B: %q", status, got)
			}
		})
	}
}

func appendPlanStopRecord(t *testing.T, path string, record map[string]any) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func planStop(t *testing.T, repo, transcript string, nested bool) string {
	t.Helper()
	event := map[string]any{"cwd": repo, "session_id": "plan-stop-session", "transcript_path": transcript, "stop_hook_active": nested}
	var output map[string]any
	data := Stop(event, time.Now())
	if len(data) == 0 {
		return ""
	}
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	return str(output["reason"])
}
