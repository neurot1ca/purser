package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "What is the weather?"}})
	first := planStop(t, repo, transcript, false)
	wantReason := "Continue accepted plan plan-1 at milestone 1 of 2: dispatch the next pass through normal Purser routing and review. " + renderActivePlanPointer(repo)
	if !strings.Contains(first, wantReason) {
		t.Fatalf("first Stop reason = %q, want to contain %q", first, wantReason)
	}
	if duplicate := planStop(t, repo, transcript, false); duplicate != "" {
		t.Fatalf("duplicate Stop blocked same user turn: %q", duplicate)
	}
	if nested := planStop(t, repo, transcript, true); nested != "" {
		t.Fatalf("nested Stop blocked: %q", nested)
	}

	state, err = UpdatePlan(root, repo, "plan-1", state.Revision+2, "complete-milestone", "")
	if err != nil || state.CurrentMilestone != 2 {
		t.Fatalf("complete milestone: state=%+v err=%v", state, err)
	}
	appendPlanStopRecord(t, transcript, map[string]any{"type": "user", "message": map[string]any{"content": "Another unrelated question."}})
	if next := planStop(t, repo, transcript, false); !strings.Contains(next, "milestone 2 of 2: dispatch the next pass") {
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
	if resumed := planStop(t, repo, transcript, false); !strings.Contains(resumed, "milestone 2 of 2: dispatch the next pass") {
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
