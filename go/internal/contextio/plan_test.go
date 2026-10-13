package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const readyPlanArtifact = `## Milestones
### M1 - prerequisite
**Dependency:** none. **Parallel group:** A.
**Write paths:**
- src/a.go
**Implementation:** bounded task one
### M2 - independent
**Dependency:** none
**Write paths:**
- src/b.go
**Implementation:** bounded task two
### M3 - dependent
**Dependency:** M1
**Write paths:**
- src/a.go
**Implementation:** bounded task three
## Other material
This must not be part of M3's task section.
`

func TestParsePlanMilestonesBulletInlineDeclarations(t *testing.T) {
	artifact := []byte("## Milestones\n" +
		"### M1\n- **Dependency:** None.\n- **Write paths:** `go/internal/testutil/environment.go` only.\n- **Validation:** focused test.\n- **Parallel group:** P1\n" +
		"### M2\n  * **Dependency:** none\n  * **Touched paths:** `go/internal/testutil/other.go` only.\n  * **Validation:** focused test.\n  * **Parallel group:** P1\n")
	milestones, err := parsePlanMilestones(artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(milestones) != 2 {
		t.Fatalf("got %d milestones, want 2", len(milestones))
	}
	for i, want := range []string{"go/internal/testutil/environment.go", "go/internal/testutil/other.go"} {
		if len(milestones[i].Dependencies) != 0 {
			t.Errorf("%s dependencies = %v, want none", milestones[i].ID, milestones[i].Dependencies)
		}
		if !reflect.DeepEqual(milestones[i].WritePaths, []string{want}) {
			t.Errorf("%s write paths = %v, want [%s]", milestones[i].ID, milestones[i].WritePaths, want)
		}
	}
}

func TestParsePlanMilestonesLegacyAndBulletDependency(t *testing.T) {
	legacy, err := parsePlanMilestones([]byte(readyPlanArtifact), []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 3 || !reflect.DeepEqual(legacy[0].WritePaths, []string{"src/a.go"}) {
		t.Fatalf("legacy declaration did not parse: %+v", legacy)
	}
	bullet, err := parsePlanMilestones([]byte("## Milestones\n### M1\n- **Dependency:** none\n- **Write paths:**\n  - `src/a.go`\n### M2\n- **Dependency:** M1\n- **Write paths:**\n  - `src/b.go`\n"), []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(bullet) != 2 || !reflect.DeepEqual(bullet[1].Dependencies, []int{1}) {
		t.Fatalf("bullet dependency = %+v, want M2 depends on M1", bullet)
	}
}

func TestPlanGraphRejectsInvalidAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, artifact string
		accepted       []int
	}{
		{"cycle", strings.Replace(readyPlanArtifact, "none. **Parallel group:** A.", "M3", 1), []int{1, 2, 3}},
		{"missing dependency", strings.Replace(readyPlanArtifact, "**Dependency:** M1", "**Dependency:** M9", 1), []int{1, 2, 3}},
		{"unaccepted prerequisite", readyPlanArtifact, []int{3}},
		{"missing accepted ID", readyPlanArtifact, []int{1, 4}},
		{"partial declaration", strings.Replace(readyPlanArtifact, "**Dependency:** M1\n", "", 1), []int{1, 2, 3}},
		{"duplicate declaration", strings.Replace(readyPlanArtifact, "**Dependency:** M1", "**Dependency:** M1\n**Dependency:** none", 1), []int{1, 2, 3}},
		{"invalid dependency suffix", strings.Replace(readyPlanArtifact, "**Dependency:** M1", "**Dependency:** M1.M9", 1), []int{1, 2, 3}},
		{"duplicate ID", strings.Replace(readyPlanArtifact, "### M2", "### M1", 1), []int{1, 2, 3}},
		{"parallel scope", strings.Replace(readyPlanArtifact, "src/b.go", "src", 1), []int{1, 2, 3}},
		{"unsafe path", strings.Replace(readyPlanArtifact, "src/b.go", "../outside", 1), []int{1, 2, 3}},
		{"unbounded section", readyPlanArtifact + "### M4\n**Dependency:** none\n**Write paths:**\n- c.go\n" + strings.Repeat("x", 32769), []int{1, 2, 3, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repo, artifact := planFixture(t)
			if err := os.WriteFile(artifact, []byte(tc.artifact), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptPlan(root, repo, "graph", artifact, tc.accepted); err == nil {
				t.Fatal("invalid graph accepted")
			}
			path, _, _ := planPath(root, repo)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("acceptance wrote invalid state: %v", err)
			}
		})
	}
}

func TestPlanReadyLeavesAndImmediateDependentRelease(t *testing.T) {
	root, repo, artifact := planFixture(t)
	if err := os.WriteFile(artifact, []byte(readyPlanArtifact), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := AcceptPlan(root, repo, "graph", artifact, []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ReadyMilestones) != 2 || state.ReadyMilestones[0].ID != "M1" || state.ReadyMilestones[1].ID != "M2" {
		t.Fatalf("independent leaves: %+v", state)
	}
	if strings.Contains(state.Milestones[2].TaskSection, "Other material") {
		t.Fatal("task section leaked beyond milestone")
	}
	writeJob := func(id, task, status string) {
		t.Helper()
		path := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(map[string]any{"jobId": id, "taskId": task, "state": status})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeJob("first", "graph-m1of3", "SUCCEEDED")
	if _, _, err := BindPlanJob(root, repo, "graph-m1of3", "first"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlanForTask(root, repo, "graph")
	if err != nil || len(state.ReadyMilestones) != 1 || state.ReadyMilestones[0].ID != "M2" {
		t.Fatalf("linked result was offered again: %+v %v", state, err)
	}
	writeJob("independent", "graph-m2of3", "RUNNING")
	if _, _, err := BindPlanJob(root, repo, "graph-m2of3", "independent"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BindPlanJob(root, repo, "graph-m3of3", "premature"); err == nil {
		t.Fatal("unready dependency bound")
	} else {
		var readinessErr *PlanReadinessError
		if !errors.Is(err, ErrPlanNotReady) || !errors.As(err, &readinessErr) || !reflect.DeepEqual(readinessErr.MissingDependencies, []string{"M1"}) {
			t.Fatalf("readiness refusal = %v (%+v)", err, readinessErr)
		}
	}
	if err := RecordPlanApply(root, repo, "graph-m1of3", "first"); err != nil {
		t.Fatal(err)
	}
	if err := RecordPlanValidation(root, repo, "graph-m1of3", "first", "PENDING"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlanForTask(root, repo, "graph")
	if err != nil || len(state.ReadyMilestones) != 0 || containsMilestone(state.CompletedMilestones, 1) {
		t.Fatalf("pending host validation released dependent: %+v %v", state, err)
	}
	if err := RecordPlanValidation(root, repo, "graph-m1of3", "first", "SKIPPED"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlanForTask(root, repo, "graph")
	if err != nil || len(state.ReadyMilestones) != 0 || containsMilestone(state.CompletedMilestones, 1) {
		t.Fatalf("skipped host validation completed prerequisite: %+v %v", state, err)
	}
	if err := RecordPlanValidation(root, repo, "graph-m1of3", "first", "PASS"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlanForTask(root, repo, "graph")
	if err != nil || state.CurrentMilestone == 0 || len(state.ReadyMilestones) != 1 || state.ReadyMilestones[0].ID != "M3" || len(state.LinkedJobs) != 1 {
		t.Fatalf("dependent not released alongside independent work: %+v %v", state, err)
	}
	if !containsMilestone(state.CompletedMilestones, 1) {
		t.Fatalf("passing host validation did not complete prerequisite: %+v", state)
	}
	if state.ReadyMilestones[0].ArtifactRef != state.ArtifactPath+"#M3" {
		t.Fatal("missing artifact pointer")
	}
	for _, value := range []string{"", "1"} {
		repeated, err := UpdatePlan(root, repo, "graph", state.Revision, "complete-milestone", value)
		if err != nil || repeated.Revision != state.Revision || !reflect.DeepEqual(repeated.CompletedMilestones, []int{1}) {
			t.Fatalf("apply follow-up completed successor: %+v %v", repeated, err)
		}
	}
	if _, err := UpdatePlan(root, repo, "graph", state.Revision, "complete-milestone", "2"); err == nil {
		t.Fatal("running milestone completed")
	}
	// The validation seam blocks dependent release whenever a state is present
	// and not PASS; absence retains existing completion behavior.
	path, _, _ := planPath(root, repo)
	for _, validation := range []string{"PENDING", "FAIL", "UNKNOWN", "SKIPPED"} {
		state.Milestones[0].ValidationState = validation
		if err := writePlans(path, []*PlanState{state}); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPlanForTask(root, repo, "graph")
		if err != nil || len(got.ReadyMilestones) != 0 {
			t.Fatalf("validation %s released dependent: %+v %v", validation, got, err)
		}
	}
	state.Milestones[0].ValidationState = "PASS"
	if err := writePlans(path, []*PlanState{state}); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"stop", "gate"} {
		state, err = UpdatePlan(root, repo, "graph", state.Revision, operation, "approval")
		if err != nil || len(state.ReadyMilestones) != 0 {
			t.Fatalf("%s plan offered work: %+v %v", operation, state, err)
		}
		if operation == "stop" {
			state, err = UpdatePlan(root, repo, "graph", state.Revision, "resume", "")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPlanReadyEarlierPlanWriteOverlap(t *testing.T) {
	root, repo, artifact := planFixture(t)
	if err := os.WriteFile(artifact, []byte(readyPlanArtifact), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := AcceptPlan(root, repo, "earlier", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := AcceptPlan(root, repo, "later", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.ReadyMilestones) != 1 || b.ReadyMilestones[0].ID != "M2" {
		t.Fatalf("overlapping milestone ready on acceptance: %+v", b.ReadyMilestones)
	}
	read, err := ReadPlanForTask(root, repo, b.TaskID)
	if err != nil || !reflect.DeepEqual(read.ReadyMilestones, b.ReadyMilestones) {
		t.Fatalf("read readiness: %+v %v", read, err)
	}
	ready, err := PlanReadiness(root, repo, "later-m1of2")
	if err != nil || !reflect.DeepEqual(ready, b.ReadyMilestones) {
		t.Fatalf("dispatch readiness: %+v %v", ready, err)
	}
	plans, err := ReconcilePlans(root, repo)
	if err != nil || len(plans) != 2 || len(plans[0].ReadyMilestones) != 1 || !reflect.DeepEqual(plans[1].ReadyMilestones, b.ReadyMilestones) {
		t.Fatalf("Stop readiness / earlier priority: %+v %v", plans, err)
	}
	if _, err := UpdatePlan(root, repo, a.TaskID, a.Revision, "complete-milestone", "1"); err != nil {
		t.Fatal(err)
	}
	read, err = ReadPlanForTask(root, repo, b.TaskID)
	if err != nil || len(read.ReadyMilestones) != 2 || read.ReadyMilestones[0].ID != "M1" || read.ReadyMilestones[1].ID != "M2" {
		t.Fatalf("completion did not release overlap: %+v %v", read, err)
	}
}

func TestPlanReadyEarlierUserStopped(t *testing.T) {
	root, repo, artifact := planFixture(t)
	if err := os.WriteFile(artifact, []byte(readyPlanArtifact), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := AcceptPlan(root, repo, "earlier", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdatePlan(root, repo, a.TaskID, a.Revision, "stop", ""); err != nil {
		t.Fatal(err)
	}
	b, err := AcceptPlan(root, repo, "later", artifact, []int{1})
	if err != nil || len(b.ReadyMilestones) != 1 || b.ReadyMilestones[0].ID != "M1" {
		t.Fatalf("stopped earlier plan blocked milestone: %+v %v", b, err)
	}
}

func TestRecordPlanValidationRejectsUnknownStatus(t *testing.T) {
	root, repo, artifact := planFixture(t)
	if err := os.WriteFile(artifact, []byte(readyPlanArtifact), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "graph", artifact, []int{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := RecordPlanValidation(root, repo, "graph-m1of3", "missing", "NOT_A_STATUS"); err == nil {
		t.Fatal("unknown validation status was accepted")
	}
	state, err := ReadPlanForTask(root, repo, "graph")
	if err != nil {
		t.Fatal(err)
	}
	if state.Milestones[0].ValidationState != "" {
		t.Fatalf("unknown validation status was stored: %+v", state.Milestones[0])
	}
}

func TestLaterPassingValidationSettlesStaleMilestone(t *testing.T) {
	for _, priorState := range []string{"STALE", "INTERRUPTED"} {
		t.Run(priorState, func(t *testing.T) {
			root, repo := planValidationSettlementFixture(t, priorState)
			if err := RecordPlanValidation(root, repo, "graph-m2of3", "job-2", "PASS"); err != nil {
				t.Fatal(err)
			}
			state, err := ReadPlanForTask(root, repo, "graph")
			if err != nil {
				t.Fatal(err)
			}
			if state.Milestones[0].ValidationState != "PASS" {
				t.Fatalf("%s prerequisite state = %q, want PASS", priorState, state.Milestones[0].ValidationState)
			}
			if !containsMilestone(state.CompletedMilestones, 1) {
				t.Fatalf("passing later validation did not complete prerequisite: %+v", state)
			}
		})
	}
}

func TestLaterFailingValidationSettlesStaleMilestoneAsFailed(t *testing.T) {
	root, repo := planValidationSettlementFixture(t, "STALE")
	if err := RecordPlanValidation(root, repo, "graph-m2of3", "job-2", "FAILED"); err != nil {
		t.Fatal(err)
	}
	state, err := ReadPlanForTask(root, repo, "graph")
	if err != nil {
		t.Fatal(err)
	}
	if state.Milestones[0].ValidationState != "FAILED" {
		t.Fatalf("stale prerequisite state = %q, want FAILED", state.Milestones[0].ValidationState)
	}
	if containsMilestone(state.CompletedMilestones, 1) || planDependenciesDone(state, 3) {
		t.Fatalf("failed later validation released prerequisite: %+v", state)
	}
}

func TestLaterPassingValidationDoesNotClearFailedMilestone(t *testing.T) {
	root, repo := planValidationSettlementFixture(t, "FAILED")
	if err := RecordPlanValidation(root, repo, "graph-m2of3", "job-2", "PASS"); err != nil {
		t.Fatal(err)
	}
	state, err := ReadPlanForTask(root, repo, "graph")
	if err != nil {
		t.Fatal(err)
	}
	if state.Milestones[0].ValidationState != "FAILED" || planDependenciesDone(state, 3) {
		t.Fatalf("later pass cleared genuinely failed prerequisite: %+v", state)
	}
}

func planValidationSettlementFixture(t *testing.T, firstState string) (string, string) {
	t.Helper()
	root, repo, artifact := planFixture(t)
	if err := os.WriteFile(artifact, []byte(readyPlanArtifact), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptPlan(root, repo, "graph", artifact, []int{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	for _, pass := range []struct{ task, job string }{{"graph-m1of3", "job-1"}, {"graph-m2of3", "job-2"}} {
		if _, _, err := BindPlanJob(root, repo, pass.task, pass.job); err != nil {
			t.Fatal(err)
		}
		if err := RecordPlanApply(root, repo, pass.task, pass.job); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordPlanValidation(root, repo, "graph-m1of3", "job-1", firstState); err != nil {
		t.Fatal(err)
	}
	return root, repo
}

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
	b, matched, err = BindPlanJob(root, repo, "plan-b-m1of2-p2", "job-b-recovery")
	if err != nil || !matched {
		t.Fatalf("bind B recovery job: %+v %v %v", b, matched, err)
	}
	if err := RecordPlanApply(root, repo, "plan-b-m1of2-p1", "job-b-apply"); err != nil {
		t.Fatal(err)
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

func TestPlanPassDoesNotCompleteMilestoneAutomatically(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = BindPlanJob(root, repo, "plan-1-m1of2-p1", "job-p1")
	if err != nil {
		t.Fatal(err)
	}
	if err = RecordPlanApply(root, repo, "plan-1-m1of2-p1", "job-p1"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlan(root, repo)
	if err != nil || state.CurrentMilestone != 1 || state.PendingNextAction != "dispatch" || len(state.CompletedMilestones) != 0 {
		t.Fatalf("pass apply completed milestone: %+v, %v", state, err)
	}
}

func TestPlanPlainMilestoneApplyCompletes(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if state, _, err = BindPlanJob(root, repo, "plan-1-m1of2", "job-milestone"); err != nil {
		t.Fatal(err)
	}
	if err = RecordPlanApply(root, repo, "plan-1-m1of2", "job-milestone"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlan(root, repo)
	if err != nil || state.CurrentMilestone != 2 || len(state.CompletedMilestones) != 1 || state.CompletedMilestones[0] != 1 {
		t.Fatalf("plain milestone apply did not complete: %+v, %v", state, err)
	}
}

func TestPlanParallelDispatchesBothRecord(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	_, matched, err := BindPlanJob(root, repo, "plan-1-m1of1-p1", "job-a")
	if err != nil || !matched {
		t.Fatalf("first dispatch matched=%v err=%v", matched, err)
	}
	state, matched, err = BindPlanJob(root, repo, "plan-1-m1of1-p2c", "job-b")
	if err != nil || !matched || len(state.LinkedJobs) != 2 || state.PendingNextAction != "apply" {
		t.Fatalf("parallel dispatch was not linked: state=%+v matched=%v err=%v", state, matched, err)
	}
	if err = RecordPlanApply(root, repo, "plan-1-m1of1-p1", "job-a"); err != nil {
		t.Fatal(err)
	}
	if err = RecordPlanApply(root, repo, "plan-1-m1of1-p2c", "job-b"); err != nil {
		t.Fatal(err)
	}
	state, err = ReadPlanForTask(root, repo, "plan-1")
	if err != nil || len(state.LinkedJobs) != 0 || state.CurrentMilestone != 1 || state.PendingNextAction != "dispatch" || len(state.AppliedPasses) != 2 || len(state.CompletedMilestones) != 0 {
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
	state, _, err = BindPlanJob(root, repo, "plan-1-m1of1", "job-applied")
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{"jobId": "job-applied", "taskId": "plan-1-m1of1", "state": "SUCCEEDED", "applyState": "applied"})
	if err := os.WriteFile(filepath.Join(jobDir, "job-applied.json"), job, 0600); err != nil {
		t.Fatal(err)
	}
	state, err = ReconcilePlanJob(root, repo)
	if err != nil || len(state.LinkedJobs) != 0 || state.CurrentMilestone != 0 || state.PendingNextAction != "" || len(state.AppliedPasses) != 1 || len(state.CompletedMilestones) != 1 {
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
	} else if err.Error() != "milestone 1 has a running linked job job-open" {
		t.Fatalf("completion error = %q", err)
	}
}

func TestPlanCompleteMilestoneClearsSupersededAndMissingJobs(t *testing.T) {
	root, repo, artifact := planFixture(t)
	state, err := AcceptPlan(root, repo, "plan-1", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = BindPlanJob(root, repo, "plan-1-m1of2-p1c", "job-superseded")
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = BindPlanJob(root, repo, "plan-1-m1of2-p2c", "job-missing")
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{
		"jobId": "job-superseded", "taskId": "plan-1-m1of2-p1c", "state": "SUCCEEDED",
	})
	if err := os.WriteFile(filepath.Join(jobDir, "job-superseded.json"), job, 0600); err != nil {
		t.Fatal(err)
	}
	updated, err := UpdatePlan(root, repo, "plan-1", state.Revision, "complete-milestone", "")
	if err != nil {
		t.Fatalf("completion rejected for terminal and missing linked jobs: %v", err)
	}
	if updated.CurrentMilestone != 2 || len(updated.CompletedMilestones) != 1 || updated.CompletedMilestones[0] != 1 {
		t.Fatalf("milestone progress = completed %v, current %d", updated.CompletedMilestones, updated.CurrentMilestone)
	}
	if got := linkedJobs(updated); len(got) != 0 || updated.PendingNextAction != "dispatch" {
		t.Fatalf("linked jobs = %v, pending action = %q", got, updated.PendingNextAction)
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
