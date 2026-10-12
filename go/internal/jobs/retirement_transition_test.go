package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/ledger"
	"purser-hook/internal/retention"
)

func TestApplyDispatchesRetirementAndPreservesCachedOutcome(t *testing.T) {
	id := strings.Repeat("a", 64)
	state, worktree := retirementApplyJob(t, id)
	if _, err := retention.AttachWorktree(state, worktree, id, "thread"); err != nil {
		t.Fatal(err)
	}
	launches := 0
	prior := launchRetirementDetached
	launchRetirementDetached = func([]string, []string, string) (int, int64, error) {
		launches++
		return 1, 0, nil
	}
	t.Cleanup(func() { launchRetirementDetached = prior })

	first := Apply(state, map[string]any{"jobId": id})
	if first["structuredContent"].(map[string]any)["applied"] != true {
		t.Fatalf("apply failed: %#v", first)
	}
	second := Apply(state, map[string]any{"jobId": id})
	if second["structuredContent"].(map[string]any)["applied"] != true {
		t.Fatalf("cached apply failed: %#v", second)
	}
	if launches != 1 {
		t.Fatalf("retirement launches = %d, want exactly one", launches)
	}
}

func TestApplyRetirementFailureKeepsSavedSuccess(t *testing.T) {
	id := strings.Repeat("b", 64)
	state, worktree := retirementApplyJob(t, id)
	if _, err := retention.AttachWorktree(state, worktree, id, "thread"); err != nil {
		t.Fatal(err)
	}
	prior := launchRetirementDetached
	launchRetirementDetached = func([]string, []string, string) (int, int64, error) {
		return 0, 0, errors.New("launcher unavailable")
	}
	t.Cleanup(func() { launchRetirementDetached = prior })

	result := Apply(state, map[string]any{"jobId": id})
	if result["structuredContent"].(map[string]any)["applied"] != true {
		t.Fatalf("dispatch failure changed apply result: %#v", result)
	}
}

func TestRetirementTransitionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		discard    bool
		lease      bool
		wantLaunch int
	}{
		{name: "discarded", state: "SUCCEEDED", discard: true, wantLaunch: 1},
		{name: "cancelled", state: "QUEUED", lease: true, wantLaunch: 1},
		{name: "unapplied delta", state: "SUCCEEDED"},
		{name: "failed", state: "FAILED"},
		{name: "running", state: "RUNNING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := strings.Repeat("c", 64)
			state, worktree := retirementApplyJob(t, id)
			if _, err := retention.AttachWorktree(state, worktree, id, "thread"); err != nil {
				t.Fatal(err)
			}
			jobPath := filepath.Join(state, "codex_exec_mcp", "jobs", id+".json")
			var job map[string]any
			encoded, err := os.ReadFile(jobPath)
			if err != nil || json.Unmarshal(encoded, &job) != nil {
				t.Fatalf("read job: %v", err)
			}
			job["state"], job["leaseQueued"] = tc.state, tc.lease
			encoded, _ = json.Marshal(job)
			write(t, jobPath, string(encoded))
			launches := 0
			prior := launchRetirementDetached
			launchRetirementDetached = func([]string, []string, string) (int, int64, error) {
				launches++
				return 1, 0, nil
			}
			t.Cleanup(func() { launchRetirementDetached = prior })
			if tc.discard || tc.lease {
				cancelled := Cancel(state, map[string]any{"jobId": id}, CancelOptions{
					RecordDecision:             func(map[string]any, string, string, string) {},
					RecordScopeConflictOutcome: func(map[string]any, string) {},
					ReleaseScopeLease:          func(string, string) bool { return false },
				})
				if tc.discard {
					applied := Apply(state, map[string]any{"jobId": id})
					if applied["structuredContent"].(map[string]any)["errorCode"] != "DISCARDED" {
						t.Fatalf("discarded apply outcome changed: %#v", applied)
					}
				}
				if tc.lease && cancelled["structuredContent"].(map[string]any)["state"] != "CANCELLED" {
					t.Fatalf("cancel outcome changed: %#v", cancelled)
				}
			}
			if launches != tc.wantLaunch {
				t.Fatalf("retirement launches = %d, want %d", launches, tc.wantLaunch)
			}
		})
	}
}

func TestCancelRetirementFailureKeepsRejectionResult(t *testing.T) {
	id := strings.Repeat("d", 64)
	state, worktree := retirementApplyJob(t, id)
	if _, err := retention.AttachWorktree(state, worktree, id, "thread"); err != nil {
		t.Fatal(err)
	}
	prior := launchRetirementDetached
	launchRetirementDetached = func([]string, []string, string) (int, int64, error) {
		return 0, 0, errors.New("launcher unavailable")
	}
	t.Cleanup(func() { launchRetirementDetached = prior })
	result := Cancel(state, map[string]any{"jobId": id}, CancelOptions{
		RecordDecision:             func(map[string]any, string, string, string) {},
		RecordScopeConflictOutcome: func(map[string]any, string) {},
		ReleaseScopeLease:          func(string, string) bool { return false },
	})
	if result["structuredContent"].(map[string]any)["state"] != "SUCCEEDED" || result["isError"] == true {
		t.Fatalf("dispatch failure changed cancel result: %#v", result)
	}
	record := readJob(filepath.Join(state, "codex_exec_mcp", "jobs"), id)
	if record == nil || recordMap(record)["applyState"] != "rejected" || recordMap(record)["discarded"] == true {
		t.Fatalf("cancel did not persist rejection without discard: %#v", recordMap(record))
	}
}

func TestDiscardedCancelRetiresWorktreeWhenChildCompletes(t *testing.T) {
	id := strings.Repeat("e", 64)
	state, worktree := retirementApplyJob(t, id)
	if _, err := retention.AttachWorktree(state, worktree, id, "thread"); err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(state, "codex_exec_mcp", "jobs", id+".json")
	var job map[string]any
	encoded, err := os.ReadFile(jobPath)
	if err != nil || json.Unmarshal(encoded, &job) != nil {
		t.Fatalf("read job: %v", err)
	}
	job["discarded"] = true
	encoded, err = json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	write(t, jobPath, string(encoded))
	prior := launchRetirementDetached
	launchRetirementDetached = func(argv, _ []string, _ string) (int, int64, error) {
		if len(argv) < 4 || argv[1] != "retire-worktree" || argv[3] != id {
			t.Fatalf("unexpected retirement command: %#v", argv)
		}
		return 1, 1, RetireWorktree(state, id)
	}
	t.Cleanup(func() { launchRetirementDetached = prior })
	result := Cancel(state, map[string]any{"jobId": id})
	if result["structuredContent"].(map[string]any)["discarded"] != true {
		t.Fatalf("cancel result = %#v", result)
	}
	if pathExists(worktree) {
		t.Fatal("discarded worktree was not retired")
	}
}

func retirementApplyJob(t *testing.T, id string) (string, string) {
	t.Helper()
	base := jobsScratchDir(t, "apply-retirement-")
	state, repo := filepath.Join(base, "state"), filepath.Join(base, "repo")
	worktree := filepath.Join(state, "wt", id)
	if err := os.MkdirAll(filepath.Dir(worktree), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "tracked.txt"), "base\n")
	git(t, repo, "add", "--all")
	git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	snapshot, err := ledger.CheckoutSnapshot(worktree)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(worktree, "tracked.txt"), "changed\n")
	job := map[string]any{"jobId": id, "state": "SUCCEEDED", "repoRoot": repo, "worktreePath": worktree, "snapshot": snapshot}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(state, "codex_exec_mcp", "jobs")
	if err = os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(jobDir, id+".json"), string(encoded))
	return state, worktree
}
