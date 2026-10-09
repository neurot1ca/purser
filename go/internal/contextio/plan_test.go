package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	before := append([]byte(nil), data...)
	if _, err := UpdatePlan(root, repo, "plan-1", 1, "stop", ""); err == nil {
		t.Fatal("mismatched project path was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("mismatched project was overwritten: %v", err)
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
	path := filepath.Join(root, "projects", before.ProjectKey, "active_plan.json")
	originalBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	originalWriter := writePlanJSONAtomically
	writePlanJSONAtomically = func(string, any) error { return errors.New("simulated interrupted replacement") }
	t.Cleanup(func() { writePlanJSONAtomically = originalWriter })
	if _, err := UpdatePlan(root, repo, "plan-1", before.Revision, "stop", ""); err == nil {
		t.Fatal("interrupted update reported success")
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(originalBytes, afterBytes) {
		t.Fatalf("failed replacement changed original bytes: %v", err)
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
	if _, err := AcceptPlan(root, repo, "another-plan", artifact, []int{1}); err != nil {
		t.Fatal(err)
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
	state, err = ReadPlanForTask(root, repo, "plan-1")
	if err != nil || state.PendingNextAction != "" {
		t.Fatalf("completed plan selection/action = %+v, %v", state, err)
	}
}

func TestPlanCollectionCoexistence(t *testing.T) {
	root, repo, artifact := planFixture(t)
	a, err := AcceptPlan(root, repo, "plan-a", artifact, []int{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	a, err = UpdatePlan(root, repo, a.TaskID, a.Revision, "complete-milestone", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a, err = UpdatePlan(root, repo, a.TaskID, a.Revision, "complete-milestone", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	if a.CurrentMilestone != 4 {
		t.Fatalf("A milestone = %d, want 4", a.CurrentMilestone)
	}
	path := filepath.Join(root, "projects", a.ProjectKey, "active_plan.json")
	legacyBytes, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	readA, err := ReadPlan(root, repo)
	if err != nil || readA.CurrentMilestone != 4 {
		t.Fatalf("read legacy A: %+v %v", readA, err)
	}
	afterLegacyRead, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(legacyBytes, afterLegacyRead) {
		t.Fatalf("legacy read migrated state: %v", err)
	}
	beforeA := *a
	b, err := AcceptPlan(root, repo, "plan-b", artifact, []int{1, 2})
	if err != nil {
		t.Fatalf("accept B alongside A: %v", err)
	}
	b, matched, err := BindPlanJob(root, repo, "plan-b-m1of2-p1", "job-b-apply")
	if err != nil || !matched {
		t.Fatalf("bind B: %+v %v %v", b, matched, err)
	}
	if err := RecordPlanApply(root, repo, "plan-b-m1of2-p1", "job-b-apply"); err != nil {
		t.Fatal(err)
	}
	b, matched, err = BindPlanJob(root, repo, "plan-b-m1of2-p2", "job-b-recovery")
	if err != nil || !matched {
		t.Fatalf("bind B recovery job: %+v %v %v", b, matched, err)
	}
	jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{"jobId": "job-b-recovery", "taskId": "plan-b-m1of2-p2", "state": "SUCCEEDED", "applyState": "applied"})
	if err := os.WriteFile(filepath.Join(jobDir, "job-b-recovery.json"), job, 0600); err != nil {
		t.Fatal(err)
	}
	b, err = ReconcilePlanJob(root, repo)
	if err != nil || b.TaskID != "plan-b" || b.CurrentMilestone != 1 {
		t.Fatalf("recover B: %+v %v", b, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Plans []PlanState `json:"plans"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	var afterA *PlanState
	for i := range envelope.Plans {
		if envelope.Plans[i].TaskID == "plan-a" {
			afterA = &envelope.Plans[i]
		}
	}
	if afterA == nil || !reflect.DeepEqual(beforeA, *afterA) {
		t.Fatalf("A changed during B operations: before=%+v after=%+v", beforeA, afterA)
	}
	if b.PendingNextAction == "" {
		t.Fatal("unfinished B has empty pending action")
	}
	if len(b.AppliedPasses) != 2 {
		t.Fatalf("B apply/recovery records = %+v", b.AppliedPasses)
	}
}

func TestPlanPassesRequireExplicitMilestoneCompletion(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	for i, task := range []string{"plan-1-m1of2-p1c", "plan-1-m1of2-p2c"} {
		state, _, err = BindPlanJob(root, repo, task, string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		if state.CurrentMilestone != 1 {
			t.Fatalf("dispatch advanced milestone: %+v", state)
		}
		if err = RecordPlanApply(root, repo, task, string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
		before, err := ReadPlan(root, repo)
		if err != nil {
			t.Fatal(err)
		}
		if before.CurrentMilestone != 1 || len(before.CompletedMilestones) != 0 || before.PendingNextAction != "dispatch" || len(before.LinkedJobs) != 0 {
			t.Fatalf("apply completed milestone: %+v", before)
		}
		if err = RecordPlanApply(root, repo, task, string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
		after, _ := ReadPlan(root, repo)
		if after.Revision != before.Revision || len(after.AppliedPasses) != i+1 {
			t.Fatalf("duplicate apply changed progress: %+v", after)
		}
		state = after
	}
	state, err = UpdatePlan(root, repo, "plan-1", state.Revision, "complete-milestone", "")
	if err != nil || state.CurrentMilestone != 2 || len(state.CompletedMilestones) != 1 {
		t.Fatalf("explicit completion = %+v, %v", state, err)
	}
	if _, err = UpdatePlan(root, repo, "plan-1", state.Revision-1, "complete-milestone", ""); err == nil {
		t.Fatal("completion replay was accepted")
	}
}

func TestPlanParallelDispatchesBothRecord(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	_, matched, err := BindPlanJob(root, repo, "plan-1-m1of1", "job-a")
	if err != nil || !matched {
		t.Fatalf("first dispatch matched=%v err=%v", matched, err)
	}
	state, matched, err = BindPlanJob(root, repo, "plan-1-m1of1-p2c", "job-b")
	if err != nil || !matched || len(state.LinkedJobs) != 2 || state.PendingNextAction != "apply" {
		t.Fatalf("parallel dispatch was not linked: state=%+v matched=%v err=%v", state, matched, err)
	}
	if err = RecordPlanApply(root, repo, "plan-1-m1of1", "job-a"); err != nil {
		t.Fatal(err)
	}
	if err = RecordPlanApply(root, repo, "plan-1-m1of1-p2c", "job-b"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlan(root, repo)
	if err != nil || len(state.LinkedJobs) != 0 || state.CurrentMilestone != 1 || state.PendingNextAction != "dispatch" || len(state.AppliedPasses) != 2 {
		t.Fatalf("parallel apply = %+v, %v", state, err)
	}
}

func TestPlanBindsNonCurrentFixPasses(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	state, matched, err := BindPlanJob(root, repo, "plan-1-m2of2-p2", "job-p2")
	if err != nil || !matched {
		t.Fatalf("non-current pass matched=%v err=%v", matched, err)
	}
	state, matched, err = BindPlanJob(root, repo, "plan-1-m2of2-p2-fix", "job-p2-fix")
	if err != nil || !matched || state.CurrentMilestone != 1 || len(state.LinkedJobs) != 2 {
		t.Fatalf("fix pass binding = %+v matched=%v err=%v", state, matched, err)
	}
}

func TestPlanRecoveryRecordsAppliedJobAfterInterruptedWrite(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = BindPlanJob(root, repo, "plan-1-m1of1-p1c", "job-applied")
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{"jobId": "job-applied", "taskId": "plan-1-m1of1-p1c", "state": "SUCCEEDED", "applyState": "applied"})
	if err := os.WriteFile(filepath.Join(jobDir, "job-applied.json"), job, 0600); err != nil {
		t.Fatal(err)
	}
	state, err = ReconcilePlanJob(root, repo)
	if err != nil || len(state.LinkedJobs) != 0 || state.CurrentMilestone != 1 || state.PendingNextAction != "dispatch" || len(state.AppliedPasses) != 1 {
		t.Fatalf("applied job recovery = %+v, %v", state, err)
	}
}

func TestPlanTerminalJobRecoveryKeepsProgress(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = BindPlanJob(root, repo, "plan-1-m1of1-p1c", "job-failed")
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{"jobId": "job-failed", "taskId": "plan-1-m1of1-p1c", "state": "FAILED"})
	if err := os.WriteFile(filepath.Join(jobDir, "job-failed.json"), job, 0600); err != nil {
		t.Fatal(err)
	}
	state, err = ReconcilePlanJob(root, repo)
	if err != nil || len(state.LinkedJobs) != 0 || state.CurrentMilestone != 1 || state.PendingNextAction != "dispatch" || len(state.AppliedPasses) != 0 {
		t.Fatalf("failed job recovery = %+v, %v", state, err)
	}
}

func TestPlanTaskIdentityAndWindowsPathSpelling(t *testing.T) {
	if !samePlanPath(`C:/projects/repo`, `C:\projects\repo`) {
		t.Fatal("Windows slash variants did not normalize")
	}
	if _, ok := planTaskMilestone("plan-1", "plan-10-m1of1"); ok {
		t.Fatal("prefix task id matched a different plan")
	}
	if milestone, ok := planTaskMilestone("plan-1", "plan-1-m3of99-p3c"); !ok || milestone != 3 {
		t.Fatalf("suffixed task id = %d, %v", milestone, ok)
	}
	for _, task := range []string{"plan-1-m1of1-p2", "plan-1-m1of1-p2-fix", "plan-1-m1of1-fix"} {
		if milestone, ok := planTaskMilestone("plan-1", task); !ok || milestone != 1 {
			t.Fatalf("fix task id %q = %d, %v", task, milestone, ok)
		}
	}
}

func TestPlanCompleteMilestoneRefusesOpenLinkedJob(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = BindPlanJob(root, repo, "plan-1-m1of1-p1c", "job-open")
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{"jobId": "job-open", "taskId": "plan-1-m1of1-p1c", "state": "RUNNING"})
	if err := os.WriteFile(filepath.Join(jobDir, "job-open.json"), job, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = UpdatePlan(root, repo, "plan-1", state.Revision, "complete-milestone", ""); err == nil {
		t.Fatal("completion accepted while a linked job was open")
	}
}

func TestPlanReadsLegacyLinkedJob(t *testing.T) {
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
	var collection planCollection
	if err = json.Unmarshal(data, &collection); err != nil || len(collection.Plans) != 1 {
		t.Fatal(err)
	}
	record := collection.Plans[0]
	record.LinkedJob = "legacy-job"
	record.LinkedJobs = nil
	record.PendingNextAction = "apply"
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlan(root, repo)
	if err != nil || state.LinkedJob != "legacy-job" || len(state.LinkedJobs) != 1 || state.LinkedJobs[0] != "legacy-job" {
		t.Fatalf("legacy link = %+v, %v", state, err)
	}
}

func TestPlanAmbiguousImplicitSelectionDoesNotMutate(t *testing.T) {
	root, repo, artifact := planFixture(t)
	first, err := AcceptPlan(root, repo, "plan-a", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "plan-b", artifact, []int{1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "projects", first.ProjectKey, "active_plan.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlan(root, repo); err == nil {
		t.Fatal("ambiguous implicit read succeeded")
	}
	if _, err := UpdatePlan(root, repo, "", first.Revision, "stop", ""); err == nil {
		t.Fatal("ambiguous implicit update succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("ambiguous selection mutated collection: %v", err)
	}
}

func TestPlanAmbiguousOwnershipDoesNotMutate(t *testing.T) {
	root, repo, artifact := planFixture(t)
	first, err := AcceptPlan(root, repo, "x", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "x-m1of1", artifact, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "projects", first.ProjectKey, "active_plan.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, matched, err := BindPlanJob(root, repo, "x-m1of1-m2of2", "job-ambiguous"); err == nil || matched {
		t.Fatalf("ambiguous owner bind = matched %v err %v", matched, err)
	}
	if err := RecordPlanApply(root, repo, "x-m1of1-m2of2", "job-ambiguous"); err == nil {
		t.Fatal("ambiguous owner apply succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("ambiguous ownership mutated collection: %v", err)
	}
}

func TestPlanDuplicateIDRejectedWithoutOverwrite(t *testing.T) {
	root, repo, artifact := planFixture(t)
	first, err := AcceptPlan(root, repo, "plan-a", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "plan-b", artifact, []int{1}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "projects", first.ProjectKey, "active_plan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var collection planCollection
	if err := json.Unmarshal(data, &collection); err != nil {
		t.Fatal(err)
	}
	collection.Plans[1].TaskID = collection.Plans[0].TaskID
	malformed, err := json.Marshal(collection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, malformed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "plan-c", artifact, []int{1}); err == nil {
		t.Fatal("duplicate IDs were accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(malformed, after) {
		t.Fatalf("duplicate rejection overwrote state: %v", err)
	}
}

func TestPlanUnsupportedVersionRejectedWithoutOverwrite(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-a", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "projects", state.ProjectKey, "active_plan.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var collection map[string]any
	if err := json.Unmarshal(data, &collection); err != nil {
		t.Fatal(err)
	}
	collection["version"] = float64(99)
	unsupported, err := json.Marshal(collection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, unsupported, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "plan-b", artifact, []int{1}); err == nil {
		t.Fatal("unsupported version was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(unsupported, after) {
		t.Fatalf("unsupported state was overwritten: %v", err)
	}
}
