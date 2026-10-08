package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func planFixture(t *testing.T) (string, string, string) {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(base, "plan.md")
	if err := os.WriteFile(artifact, []byte("accepted plan"), 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "state"), repo, artifact
}

func TestPlanInvalidArtifactBinding(t *testing.T) {
	root, repo, artifact := planFixture(t)
	if _, err := AcceptPlan(root, repo, "plan-1", filepath.Join(filepath.Dir(artifact), "missing.md"), []int{1}); err == nil {
		t.Fatal("missing artifact was accepted")
	}
	if _, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("changed plan"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlan(root, repo); err == nil {
		t.Fatal("artifact digest mismatch was accepted")
	}
}

func TestPlanProjectPathMismatch(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "projects", state.ProjectKey, "active_plan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state.ProjectRoot = filepath.Join(filepath.Dir(repo), "other")
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdatePlan(root, repo, "plan-1", 1, "stop", ""); err == nil {
		t.Fatal("mismatched project path was accepted")
	}
}

func TestPlanStaleConcurrentUpdate(t *testing.T) {
	root, repo, artifact := planFixture(t)
	if _, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdatePlan(root, repo, "plan-1", 1, "stop", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdatePlan(root, repo, "plan-1", 1, "resume", ""); err == nil {
		t.Fatal("stale concurrent update was accepted")
	}
}

func TestPlanInterruptedReplacementPreservesPrevious(t *testing.T) {
	root, repo, artifact := planFixture(t)
	before, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	originalWriter := writePlanJSONAtomically
	writePlanJSONAtomically = func(string, any) error { return errors.New("simulated interrupted replacement") }
	t.Cleanup(func() { writePlanJSONAtomically = originalWriter })
	if _, err := UpdatePlan(root, repo, "plan-1", before.Revision, "stop", ""); err == nil {
		t.Fatal("interrupted update reported success")
	}
	after, err := ReadPlan(root, repo)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.UserStopped {
		t.Fatalf("previous record changed: %+v", after)
	}
}

func TestPlanImplicitStopRejected(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdatePlan(root, repo, "plan-1", state.Revision, "implicit-stop", ""); err == nil {
		t.Fatal("implicit stop operation was accepted")
	}
	after, err := ReadPlan(root, repo)
	if err != nil {
		t.Fatal(err)
	}
	if after.UserStopped {
		t.Fatal("rejected operation set explicit user-stop flag")
	}
}

func TestPlanAcceptStopResumeCompleteMilestone(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "another-plan", artifact, []int{1}); err == nil {
		t.Fatal("another plan replaced an unfinished plan")
	}
	state, err = UpdatePlan(root, repo, "plan-1", state.Revision, "stop", "")
	if err != nil || !state.UserStopped {
		t.Fatalf("stop = %+v, %v", state, err)
	}
	state, err = UpdatePlan(root, repo, "plan-1", state.Revision, "resume", "")
	if err != nil || state.UserStopped || state.CurrentMilestone != 1 {
		t.Fatalf("resume = %+v, %v", state, err)
	}
	state, err = UpdatePlan(root, repo, "plan-1", state.Revision, "complete-milestone", "")
	if err != nil || state.CurrentMilestone != 2 || len(state.CompletedMilestones) != 1 || state.CompletedMilestones[0] != 1 {
		t.Fatalf("complete milestone = %+v, %v", state, err)
	}
	state, err = UpdatePlan(root, repo, "plan-1", state.Revision, "complete-milestone", "")
	if err != nil || state.CurrentMilestone != 0 {
		t.Fatalf("final completion = %+v, %v", state, err)
	}
}
