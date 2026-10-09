package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"purser-hook/internal/retention"
)

func retirementID(ch byte) string { return strings.Repeat(string(ch), 64) }

func TestRetireWorktreeClearsReferencesAndPreservesNewerBinding(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "retiring")
	id := retirementID('a')
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"repoRoot": repo, "worktreePath": wt, "applyState": "applied", "snapshot": "old", "head": "old"})
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
	job := readCleanupJSON(jobPath)
	job["threadConfig"] = map[string]any{"thread_id": "thread", "cwd": wt, "repoRoot": repo, "worktreePath": filepath.Join(root, "wt", "newer"), "sandbox": "new", "config_args": []any{"sandbox_mode=old", "other=keep"}}
	if err := writeJSONRecord(jobPath, job); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(root, "wt", "newer")
	if err := os.MkdirAll(newer, 0700); err != nil {
		t.Fatal(err)
	}
	threads := filepath.Join(root, "codex_exec_mcp", "threads")
	if err := SaveRecord(threads, map[string]any{"thread_id": "thread", "cwd": wt, "repoRoot": repo, "worktreePath": wt, "snapshot": "old", "baselineSnapshot": "base", "head": "old", "sandbox": "old", "config_args": []any{"sandbox_mode=old", "other=keep"}, "extra": "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecord(threads, map[string]any{"thread_id": "newer-thread", "cwd": newer, "repoRoot": repo, "worktreePath": newer, "threadConfig": map[string]any{"worktreePath": wt, "cwd": wt, "snapshot": "old"}, "config_args": []any{}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := retention.AttachWorktree(root, wt, id, "thread"); err != nil || !ok {
		t.Fatalf("attach = %v, %v", ok, err)
	}
	if _, err := retention.MarkWorktreeRetiring(root, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.UpdateWorktreeOwnership(root, wt, func(record *retention.WorktreeOwnership) { record.Claimed = true }); err != nil {
		t.Fatal(err)
	}
	prep := filepath.Join(root, "preparations", id)
	if err := os.MkdirAll(prep, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prep, "owned"), []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(root, "codex_exec_mcp", "jobs", id+".output")
	if err := os.WriteFile(artifact, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RetireWorktree(root, id); err != nil {
		t.Fatal(err)
	}
	if pathExists(wt) || pathExists(prep) {
		t.Fatal("owned worktree or preparation remains")
	}
	if !pathExists(artifact) {
		t.Fatal("job output artifact was removed")
	}
	if err := RetireWorktree(root, id); err != nil {
		t.Fatalf("duplicate retirement: %v", err)
	}
	job = readCleanupJSON(jobPath)
	if _, ok := job["worktreePath"]; ok {
		t.Fatal("job worktree reference remains")
	}
	config := job["threadConfig"].(map[string]any)
	if config["worktreePath"] != newer || config["sandbox"] != "new" || config["cwd"] != wt {
		t.Fatalf("newer thread config changed: %#v", config)
	}
	if got := config["config_args"].([]any); len(got) != 2 {
		t.Fatalf("newer sandbox args changed: %#v", got)
	}
	thread := LoadRecord(threads, "thread")
	if thread["thread_id"] != "thread" || thread["cwd"] != repo || thread["extra"] != "keep" {
		t.Fatalf("thread identity/cwd/unrelated fields = %#v", thread)
	}
	for _, key := range []string{"worktreePath", "snapshot", "baselineSnapshot", "head", "sandbox"} {
		if _, ok := thread[key]; ok {
			t.Errorf("thread still has %s", key)
		}
	}
	if args := thread["config_args"].([]any); len(args) != 1 || args[0] != "other=keep" {
		t.Fatalf("thread sandbox config = %#v", args)
	}
	if got := LoadRecord(threads, "newer-thread"); got["worktreePath"] != newer || got["cwd"] != newer || got["threadConfig"].(map[string]any)["worktreePath"] != wt {
		t.Fatalf("newer thread binding changed: %#v", got)
	}
}

func TestRetireWorktreeRefusesUnknownNonRetiringAndNonterminal(t *testing.T) {
	root := t.TempDir()
	if err := RetireWorktree(root, retirementID('a')); err == nil {
		t.Fatal("unknown job accepted")
	}
	wt := filepath.Join(root, "wt", "item")
	if err := os.MkdirAll(wt, 0700); err != nil {
		t.Fatal(err)
	}
	id := retirementID('b')
	cleanupJob(t, root, id, "RUNNING", map[string]any{"repoRoot": root, "worktreePath": wt})
	if ok, err := retention.AttachWorktree(root, wt, id, "thread"); err != nil || !ok {
		t.Fatalf("attach = %v, %v", ok, err)
	}
	if err := RetireWorktree(root, id); err == nil {
		t.Fatal("non-retiring record accepted")
	}
	if _, err := retention.MarkWorktreeRetiring(root, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.UpdateWorktreeOwnership(root, wt, func(record *retention.WorktreeOwnership) { record.Claimed = true }); err != nil {
		t.Fatal(err)
	}
	if err := RetireWorktree(root, id); err == nil {
		t.Fatal("nonterminal attached job accepted")
	}
}

func TestRetireWorktreeRejectsForeignPathAndLeavesPartialEvidence(t *testing.T) {
	root := t.TempDir()
	foreign := t.TempDir()
	id := retirementID('c')
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"repoRoot": root, "worktreePath": foreign, "applyState": "applied"})
	if ok, err := retention.AttachWorktree(root, foreign, id, "thread"); err != nil || !ok {
		t.Fatalf("attach = %v, %v", ok, err)
	}
	if _, err := retention.MarkWorktreeRetiring(root, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.UpdateWorktreeOwnership(root, foreign, func(record *retention.WorktreeOwnership) { record.Claimed = true }); err != nil {
		t.Fatal(err)
	}
	if err := RetireWorktree(root, id); err == nil {
		t.Fatal("foreign worktree path accepted")
	}
	if !pathExists(foreign) {
		t.Fatal("foreign path was removed")
	}

	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "partial")
	partialID := retirementID('d')
	cleanupJob(t, root, partialID, "SUCCEEDED", map[string]any{"repoRoot": filepath.Join(root, "missing-repo"), "worktreePath": wt, "applyState": "applied", "generation": 3})
	if ok, err := retention.AttachWorktree(root, wt, partialID, "thread"); err != nil || !ok {
		t.Fatalf("attach = %v, %v", ok, err)
	}
	if _, err := retention.MarkWorktreeRetiring(root, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.UpdateWorktreeOwnership(root, wt, func(record *retention.WorktreeOwnership) { record.Claimed = true }); err != nil {
		t.Fatal(err)
	}
	if err := RetireWorktree(root, partialID, 3); err == nil {
		t.Fatal("invalid repository unexpectedly removed worktree")
	}
	if !pathExists(wt) {
		t.Fatal("partial failure removed worktree")
	}
	if owned, err := retention.ReadWorktreeOwnership(root, wt); err != nil || !owned.Retiring || !owned.Claimed {
		t.Fatalf("partial evidence lost: %#v, %v", owned, err)
	}
	job := readCleanupJSON(filepath.Join(root, "codex_exec_mcp", "jobs", partialID+".json"))
	if _, ok := job["worktreePath"]; ok {
		t.Fatal("references were not cleared before deletion")
	}
	job["repoRoot"] = repo
	if err := writeJSONRecord(filepath.Join(root, "codex_exec_mcp", "jobs", partialID+".json"), job); err != nil {
		t.Fatal(err)
	}
	if err := recoverRetiringWorktrees(root); err != nil {
		t.Fatal(err)
	}
	if pathExists(wt) {
		t.Fatal("maintenance did not recover the partial removal")
	}
}

func TestDispatchWorktreeRetirementDoesNotWaitForChild(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "dispatch")
	id := retirementID('e')
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"repoRoot": repo, "worktreePath": wt, "applyState": "applied"})
	if ok, err := retention.AttachWorktree(root, wt, id, "thread"); err != nil || !ok {
		t.Fatalf("attach = %v, %v", ok, err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	prior := launchRetirementDetached
	var once sync.Once
	launchRetirementDetached = func(argv, _ []string, _ string) (int, int64, error) {
		if len(argv) != 4 || argv[1] != "retire-worktree" || argv[3] != id {
			t.Errorf("unexpected child argv: %#v", argv)
		}
		go func() { once.Do(func() { close(started) }); <-finish }()
		return 1, 1, nil
	}
	t.Cleanup(func() { launchRetirementDetached = prior; close(finish) })
	done := make(chan error, 1)
	go func() { done <- DispatchWorktreeRetirement(root, id) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatch waited for child completion")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("detached child was not launched")
	}
}
