package jobs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"purser-hook/internal/retention"
)

func TestRemoveWindowsLockedFileKeepsRetryEvidence(t *testing.T) {
	root := jobsScratchDir(t, "cleanup-lock-")
	repo := cleanupRepo(t, root)
	wt := filepath.Join(root, "wt", "locked")
	if err := os.MkdirAll(wt, 0700); err != nil {
		t.Fatal(err)
	}
	id := retirementID('7')
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"repoRoot": repo, "worktreePath": wt, "applyState": "applied", "finishedAt": cleanupNow().Add(-8 * 24 * time.Hour).Unix()})
	if ok, err := retention.AttachWorktree(root, wt, id, ""); !ok || err != nil {
		t.Fatal(err)
	}
	if _, err := retention.MarkWorktreeRetiring(root, wt); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(wt, "held")
	if err := os.WriteFile(file, []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if handle != syscall.InvalidHandle {
			_ = syscall.CloseHandle(handle)
		}
	}()
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
	job := readCleanupJSON(path)
	if job == nil || job["cleanupRemoval"] == nil || !pathExists(file) {
		t.Fatal("locked removal lost evidence")
	}
	if owned, err := retention.ReadWorktreeOwnership(root, wt); err != nil || owned.WorktreeGone {
		t.Fatalf("ownership lost: %#v, %v", owned, err)
	}
	if err := syscall.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = syscall.InvalidHandle
	job["cleanupRemoval"].(map[string]any)["nextAttemptAt"] = 0
	if err := writeJSONRecord(path, job); err != nil {
		t.Fatal(err)
	}
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), true); err != nil {
		t.Fatal(err)
	}
	if pathExists(wt) || pathExists(path) {
		t.Fatal("unlocked target did not recover")
	}
}
