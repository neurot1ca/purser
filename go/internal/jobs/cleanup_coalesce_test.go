package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cleanupLaunchFixture(t *testing.T) (func() (int, int64, error), *int) {
	t.Helper()
	old := cleanupChildAlive
	oldInterval := cleanupMinimumInterval
	cleanupChildAlive = func(pid int, identity int64) bool { return pid == 42 && identity == 7 }
	cleanupMinimumInterval = 0
	t.Cleanup(func() { cleanupChildAlive = old; cleanupMinimumInterval = oldInterval })
	starts := new(int)
	return func() (int, int64, error) { *starts++; return 42, 7, nil }, starts
}

func TestCleanupMinimumIntervalDefersDirtyRequest(t *testing.T) {
	root := jobsScratchDir(t, "cleanup-")
	launch, _ := cleanupLaunchFixture(t)
	oldClock, oldSleep, oldInterval := cleanupClock, cleanupSleep, cleanupMinimumInterval
	defer func() { cleanupClock, cleanupSleep, cleanupMinimumInterval = oldClock, oldSleep, oldInterval }()
	current := time.Unix(2_000_000_000, 0)
	cleanupMinimumInterval = 10 * time.Minute
	cleanupClock = func() time.Time { return current }
	slept := time.Duration(0)
	cleanupSleep = func(delay time.Duration) { slept += delay; current = current.Add(delay) }
	if err := RequestCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	passes := 0
	pass := func() error { passes++; return nil }
	if err := runCleanup(root, launch, pass); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if err := RequestCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	if err := runCleanup(root, launch, pass); err != nil {
		t.Fatal(err)
	}
	if passes != 2 || slept < 9*time.Minute {
		t.Fatalf("passes=%d slept=%s", passes, slept)
	}
}

func TestCleanupMaintenanceBeforeFinalization(t *testing.T) {
	root := jobsScratchDir(t, "cleanup-")
	t.Setenv("GOCACHE", filepath.Join(root, "external-cache"))
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "pending")
	id := strings.Repeat("a", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"worktreePath": wt, "repoRoot": repo, "applyState": "pending"})
	index := filepath.Join(root, "async_jobs", id+".json")
	if err := atomicWrite(index, []byte(`{"jobId":"`+id+`","stage":"FINALIZING"}`)); err != nil {
		t.Fatal(err)
	}
	launch, _ := cleanupLaunchFixture(t)
	if err := RequestCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	passes := 0
	pass := func() error { passes++; return cleanupJobs(root, jobsScratchDir(t, "cleanup-"), time.Now(), true) }
	if err := runCleanup(root, launch, pass); err != nil {
		t.Fatal(err)
	}
	if passes != 1 {
		t.Fatal("cleanup pass did not run")
	}
	for _, path := range []string{wt, index, filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected evidence removed: %s: %v", path, err)
		}
	}
	// Final disposition makes the same worktree eligible without any restart.
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"worktreePath": wt, "repoRoot": repo, "applyState": "applied"})
	if err := RequestCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	if err := runCleanup(root, launch, pass); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("completed worktree retained: %v", err)
	}
}

func TestCleanupCoalesceRequestDuringPass(t *testing.T) {
	root := jobsScratchDir(t, "cleanup-")
	launch, starts := cleanupLaunchFixture(t)
	if err := RequestCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	passes := 0
	if err := runCleanup(root, launch, func() error {
		passes++
		if passes == 1 {
			// A second child cannot overlap the pass, even if invoked directly.
			if err := runCleanup(root, launch, func() error { t.Fatal("overlapping sweep"); return nil }); err == nil {
				t.Fatal("second child acquired sweep lock")
			}
		}
		requested := make(chan error, 1)
		go func() { requested <- RequestCleanup(root, launch) }()
		if err := <-requested; err != nil {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if passes != 2 || *starts != 2 {
		t.Fatalf("passes=%d launches=%d", passes, *starts)
	}
	if err := runCleanup(root, launch, func() error { passes++; return nil }); err != nil {
		t.Fatal(err)
	}
	if passes != 3 || *starts != 2 {
		t.Fatalf("handoff lost request: passes=%d launches=%d", passes, *starts)
	}
}

func TestCleanupChildLaunchFailure(t *testing.T) {
	root := jobsScratchDir(t, "cleanup-")
	if err := RequestCleanup(root, func() (int, int64, error) { return 0, 0, errors.New("launch failed") }); err == nil {
		t.Fatal("launch failure hidden")
	}
	for _, name := range []string{"dirty", "error"} {
		if data, err := os.ReadFile(filepath.Join(root, "cleanup", name)); err != nil || len(data) == 0 {
			t.Fatalf("missing retry marker %s: %v", name, err)
		}
	}
	launch, starts := cleanupLaunchFixture(t)
	if err := RequestCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	passes := 0
	if err := runCleanup(root, launch, func() error { passes++; return nil }); err != nil {
		t.Fatal(err)
	}
	if *starts != 1 || passes != 1 {
		t.Fatalf("retry: launches=%d passes=%d", *starts, passes)
	}
}

func TestCleanupMaintenanceStateRoot(t *testing.T) {
	root, other := jobsScratchDir(t, "cleanup-"), jobsScratchDir(t, "cleanup-")
	t.Setenv("PURSER_STATE_DIR", other)
	t.Setenv("GOCACHE", filepath.Join(other, "external-cache"))
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "completed")
	cleanupJob(t, root, strings.Repeat("b", 64), "SUCCEEDED", map[string]any{"worktreePath": wt, "repoRoot": repo, "applyState": "applied"})
	launch, _ := cleanupLaunchFixture(t)
	if err := RequestCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	if err := RunCleanup(root, launch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("requested root was not swept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "cleanup")); !os.IsNotExist(err) {
		t.Fatalf("environment root touched: %v", err)
	}
}
