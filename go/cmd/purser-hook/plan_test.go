package main

import "purser-hook/internal/testutil"

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"purser-hook/internal/contextio"
)

func TestPlanCLICompleteTargetsAppliedMilestone(t *testing.T) {
	root, repo, artifact := planCLIFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	if _, err := contextio.AcceptPlan(root, repo, "plan", artifact, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := contextio.BindPlanJob(root, repo, "plan-m1of2", "applied"); err != nil {
		t.Fatal(err)
	}
	if err := contextio.RecordPlanApply(root, repo, "plan-m1of2", "applied"); err != nil {
		t.Fatal(err)
	}
	state, err := contextio.ReadPlanForTask(root, repo, "plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range [][]string{nil, {"--milestone", "1"}} {
		args := append([]string{"complete-milestone", "--cwd", repo, "--task-id", "plan", "--revision", strconv.Itoa(state.Revision)}, selector...)
		output, stderr, code := runPlanCLI(t, args...)
		var after contextio.PlanState
		if err := json.Unmarshal(output, &after); err != nil {
			t.Fatalf("decode: %v stderr=%q", err, stderr)
		}
		if code != 0 || after.CurrentMilestone != 2 || after.Revision != state.Revision || len(after.CompletedMilestones) != 1 {
			t.Fatalf("successor completed: %s %s", output, stderr)
		}
	}
	output, stderr, code := runPlanCLI(t, "complete-milestone", "--cwd", repo, "--task-id", "plan", "--revision", strconv.Itoa(state.Revision), "--milestone", "2")
	if code != 0 || !bytes.Contains(output, []byte(`"current_milestone":0`)) {
		t.Fatalf("no-delta milestone refused: %s %s", output, stderr)
	}
}

func TestPlanMilestoneArguments(t *testing.T) {
	values, err := parseMilestones("1, 3,5")
	if err != nil || len(values) != 3 || values[0] != 1 || values[1] != 3 || values[2] != 5 {
		t.Fatalf("parseMilestones = %v, %v", values, err)
	}
	for _, invalid := range []string{"", "0", "1,x", "-1"} {
		if _, err := parseMilestones(invalid); err == nil {
			t.Errorf("parseMilestones(%q) succeeded", invalid)
		}
	}
}

func TestPlanCLISelectsExplicitPlanAndPreservesStatusShape(t *testing.T) {
	root, repo, artifact := planCLIFixture(t)
	t.Setenv("PURSER_STATE_DIR", root)
	first, err := contextio.AcceptPlan(root, repo, "plan-a", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := contextio.AcceptPlan(root, repo, "plan-b", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	status, stderr, code := runPlanCLI(t, "status", "--cwd", repo, "--task-id", first.TaskID)
	if code != 0 || stderr != "" {
		t.Fatalf("status code=%d stderr=%q", code, stderr)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(status, &shape); err != nil {
		t.Fatalf("status is not a JSON object: %s: %v", status, err)
	}
	if _, ok := shape["task_id"]; !ok || shape["task_id"] == nil {
		t.Fatalf("status is not a single plan record: %s", status)
	}
	if _, ok := shape["plans"]; ok {
		t.Fatalf("status wrapped the plan in a collection: %s", status)
	}
	wantStatus, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(status), wantStatus) {
		t.Fatalf("status record shape changed: got %s want %s", status, wantStatus)
	}

	_, stderr, code = runPlanCLI(t, "stop", "--cwd", repo, "--task-id", first.TaskID, "--revision", "1")
	if code != 0 || stderr != "" {
		t.Fatalf("stop code=%d stderr=%q", code, stderr)
	}
	stopped, err := contextio.ReadPlanForTask(root, repo, first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	untouched, err := contextio.ReadPlanForTask(root, repo, second.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped.UserStopped || stopped.Revision != 2 {
		t.Fatalf("selected plan was not stopped: %+v", stopped)
	}
	if untouched.Revision != second.Revision || untouched.UserStopped {
		t.Fatalf("sibling plan changed: before=%+v after=%+v", second, untouched)
	}

	status, stderr, code = runPlanCLI(t, "status", "--cwd", repo, "--task-id", first.TaskID)
	if code != 0 || stderr != "" || !bytes.Contains(status, []byte(`"user_stopped":true`)) {
		t.Fatalf("selected stopped status code=%d stderr=%q output=%s", code, stderr, status)
	}
}

func TestPlanCLIImplicitSelectionRequiresOneUnfinishedPlan(t *testing.T) {
	t.Run("one unfinished", func(t *testing.T) {
		root, repo, artifact := planCLIFixture(t)
		t.Setenv("PURSER_STATE_DIR", root)
		state, err := contextio.AcceptPlan(root, repo, "only-plan", artifact, []int{1})
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runPlanCLI(t, "stop", "--cwd", repo, "--revision", "1")
		if code != 0 || stderr != "" {
			t.Fatalf("stop code=%d stderr=%q", code, stderr)
		}
		stopped, err := contextio.ReadPlanForTask(root, repo, state.TaskID)
		if err != nil || !stopped.UserStopped || stopped.Revision != 2 {
			t.Fatalf("implicit stop state=%+v err=%v", stopped, err)
		}
	})

	t.Run("zero unfinished", func(t *testing.T) {
		root, repo, artifact := planCLIFixture(t)
		t.Setenv("PURSER_STATE_DIR", root)
		state, err := contextio.AcceptPlan(root, repo, "stopped-plan", artifact, []int{1})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := contextio.UpdatePlan(root, repo, state.TaskID, state.Revision, "complete-milestone", ""); err != nil {
			t.Fatal(err)
		}
		status, stderr, code := runPlanCLI(t, "status", "--cwd", repo, "--task-id", state.TaskID)
		if code != 0 || stderr != "" || !bytes.Contains(status, []byte(`"current_milestone":0`)) {
			t.Fatalf("completed plan status code=%d stderr=%q output=%s", code, stderr, status)
		}
		_, stderr, code = runPlanCLI(t, "resume", "--cwd", repo, "--revision", "2")
		if code == 0 || !strings.Contains(stderr, "no unfinished plan") || !strings.Contains(stderr, "--task-id") {
			t.Fatalf("expected clear no-plan error, code=%d stderr=%q", code, stderr)
		}
		unchanged, err := contextio.ReadPlanForTask(root, repo, state.TaskID)
		if err != nil || unchanged.CurrentMilestone != 0 || unchanged.Revision != 2 {
			t.Fatalf("failed command mutated plan: state=%+v err=%v", unchanged, err)
		}
	})

	t.Run("multiple unfinished", func(t *testing.T) {
		root, repo, artifact := planCLIFixture(t)
		t.Setenv("PURSER_STATE_DIR", root)
		first, err := contextio.AcceptPlan(root, repo, "plan-a", artifact, []int{1})
		if err != nil {
			t.Fatal(err)
		}
		second, err := contextio.AcceptPlan(root, repo, "plan-b", artifact, []int{1})
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runPlanCLI(t, "stop", "--cwd", repo, "--revision", "1")
		if code == 0 || !strings.Contains(stderr, "multiple unfinished plans") || !strings.Contains(stderr, "--task-id") {
			t.Fatalf("expected clear ambiguity error, code=%d stderr=%q", code, stderr)
		}
		for _, before := range []*contextio.PlanState{first, second} {
			after, err := contextio.ReadPlanForTask(root, repo, before.TaskID)
			if err != nil || after.Revision != before.Revision || after.UserStopped {
				t.Fatalf("failed command mutated %s: state=%+v err=%v", before.TaskID, after, err)
			}
		}
	})
}

func planCLIFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := testutil.Hide(exec.Command("git", "-C", repo, "init", "-q")).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	artifact := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(artifact, []byte("# accepted plan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root, repo, artifact
}

func runPlanCLI(t *testing.T, args ...string) ([]byte, string, int) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	code := planMain(args)
	_ = outW.Close()
	_ = errW.Close()
	data, _ := io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)
	_ = outR.Close()
	_ = errR.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return data, string(stderr), code
}
