package enforce

import (
	"os"
	"sync"
	"testing"
)

func TestClassifyQualifyingRejection(t *testing.T) {
	base := TaskFailureInput{ProjectKey: "0123456789abcdef", TaskID: "task", JobID: "job-1", Phase: "implementation", Route: "subtle", Model: "gpt-6-luna", Outcome: "rejected", SendBackUsed: true}
	got, ok := classifyTaskFailure(base)
	if !ok || got.Category != "rejected_after_sendback" || got.JobID != base.JobID {
		t.Fatalf("qualifying rejection = %#v, %v", got, ok)
	}
}

func TestClassifyFailedAbandonment(t *testing.T) {
	base := TaskFailureInput{ProjectKey: "0123456789abcdef", TaskID: "task", JobID: "job-1", Phase: "implementation", Route: "mechanical", Model: "gpt-6-luna", Outcome: "discarded", CancellationReason: "failed"}
	got, ok := classifyTaskFailure(base)
	if !ok || got.Category != "failed_abandonment" {
		t.Fatalf("failed abandonment = %#v, %v", got, ok)
	}
}

func TestClassifyTaskFailureExcludesNonFailures(t *testing.T) {
	base := TaskFailureInput{ProjectKey: "0123456789abcdef", TaskID: "task", JobID: "job-1", Phase: "implementation", Route: "mechanical", Model: "gpt-6-luna", Outcome: "rejected", SendBackUsed: false}
	for _, change := range []func(*TaskFailureInput){
		func(v *TaskFailureInput) { v.SendBackUsed = false },
		func(v *TaskFailureInput) { v.Route = "consequential"; v.SendBackUsed = true },
		func(v *TaskFailureInput) { v.Phase = "plan"; v.SendBackUsed = true },
		func(v *TaskFailureInput) { v.Model = "sol"; v.SendBackUsed = true },
		func(v *TaskFailureInput) { v.Outcome = "cancelled"; v.CancellationReason = "user" },
		func(v *TaskFailureInput) { v.Outcome = "review_unavailable"; v.SendBackUsed = true },
		func(v *TaskFailureInput) { v.Outcome = "cancelled"; v.CancellationReason = "superseded" },
	} {
		input := base
		change(&input)
		if got, ok := classifyTaskFailure(input); ok {
			t.Errorf("non-failure classified: input=%#v evidence=%#v", input, got)
		}
	}
}

func TestTaskAttemptIdentityAndLookupStates(t *testing.T) {
	root := t.TempDir()
	a := TaskAttempt{ProjectKey: "0123456789abcdef", TaskID: "same"}
	b := TaskAttempt{ProjectKey: "fedcba9876543210", TaskID: "same"}
	if err := SaveTaskAttempt(root, a); err != nil {
		t.Fatal(err)
	}
	if _, state, err := LoadTaskAttempt(root, a.ProjectKey, a.TaskID); err != nil || state != TaskAttemptOK {
		t.Fatalf("valid lookup = %s, %v", state, err)
	}
	if _, state, err := LoadTaskAttempt(root, b.ProjectKey, b.TaskID); err != nil || state != TaskAttemptMissing {
		t.Fatalf("isolated lookup = %s, %v", state, err)
	}
	path := taskAttemptPath(root, a.ProjectKey, a.TaskID)
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, state, err := LoadTaskAttempt(root, a.ProjectKey, a.TaskID); err != nil || state != TaskAttemptCorrupt {
		t.Fatalf("corrupt lookup = %s, %v", state, err)
	}
}

func TestConcurrentTaskAttemptWritesKeepFailure(t *testing.T) {
	root := t.TempDir()
	failure := TaskFailureEvidence{JobID: "job-failed", Category: "failed_abandonment"}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			record := TaskAttempt{ProjectKey: "0123456789abcdef", TaskID: "task", LatestJobID: string(rune('a' + i))}
			if i == 0 {
				record.Failure = &failure
			}
			if err := SaveTaskAttempt(root, record); err != nil {
				t.Errorf("save: %v", err)
			}
		}(i)
	}
	wg.Wait()
	got, state, err := LoadTaskAttempt(root, "0123456789abcdef", "task")
	if err != nil || state != TaskAttemptOK || got.Failure == nil || *got.Failure != failure {
		t.Fatalf("failure lost: %#v state=%s err=%v", got, state, err)
	}
	if err := SaveTaskAttempt(root, TaskAttempt{ProjectKey: got.ProjectKey, TaskID: got.TaskID, Failure: nil}); err != nil {
		t.Fatal(err)
	}
	got, state, err = LoadTaskAttempt(root, "0123456789abcdef", "task")
	if err != nil || state != TaskAttemptOK || got.Failure == nil || *got.Failure != failure {
		t.Fatalf("subsequent update erased failure: %#v state=%s err=%v", got, state, err)
	}
}
