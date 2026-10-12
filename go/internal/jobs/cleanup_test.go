package jobs

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func TestSweepTemporaryDirectoriesKeepsLiveAndRemovesTerminal(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	tmpRoot := filepath.Join(root, "cache", "tmp")
	jobsRoot := filepath.Join(root, "codex_exec_mcp", "jobs")
	for id, state := range map[string]string{"live": "RUNNING", "done": "SUCCEEDED"} {
		dir := filepath.Join(tmpRoot, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(jobsRoot, 0700); err != nil {
			t.Fatal(err)
		}
		writeHostTestJSON(t, filepath.Join(jobsRoot, id+".json"), map[string]any{"jobId": id, "state": state})
	}
	sweepTemporaryDirectories(root, now)
	if _, err := os.Stat(filepath.Join(tmpRoot, "live")); err != nil {
		t.Fatalf("live job temp directory removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpRoot, "done")); !os.IsNotExist(err) {
		t.Fatalf("terminal job temp directory remains: %v", err)
	}
}

func TestTrimPurserGoCacheSkippedWhileCodexJobRuns(t *testing.T) {
	t.Setenv("GOCACHE", "")
	root := t.TempDir()
	cache := filepath.Join(root, "cache", "go-build")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(cache, "keep")
	if err := os.WriteFile(marker, []byte("cache"), 0600); err != nil {
		t.Fatal(err)
	}
	jobsRoot := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	writeHostTestJSON(t, filepath.Join(jobsRoot, "running.json"), map[string]any{"jobId": "running", "state": "RUNNING"})
	trimPurserGoCache(root)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cache marker removed while a Codex job runs: %v", err)
	}
}
func cleanupRepo(t *testing.T, state string) string {
	t.Helper()
	repo := filepath.Join(state, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	cleanupGit(t, repo, "init")
	cleanupGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	return repo
}
func cleanupWT(t *testing.T, state, repo, name string) string {
	t.Helper()
	p := filepath.Join(state, "wt", name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	cleanupGit(t, repo, "worktree", "add", "--detach", p, "HEAD")
	return p
}
func cleanupJob(t *testing.T, state, id, status string, extra map[string]any) {
	t.Helper()
	d := filepath.Join(state, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(d, 0700); err != nil {
		t.Fatal(err)
	}
	j := map[string]any{"jobId": id, "state": status}
	for k, v := range extra {
		j[k] = v
	}
	b, e := json.Marshal(j)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(d, id+".json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func cleanupNow() time.Time { return time.Unix(2_000_000_000, 0) }

func TestCleanupPathAllowsSymlinkedTempAncestor(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	root := filepath.Join(real, "state")
	if err := os.MkdirAll(filepath.Join(root, "wt"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if !cleanupPath(filepath.Join(alias, "state"), filepath.Join(alias, "state", "wt", "child")) {
		t.Fatal("owned path under a symlinked temporary ancestor was rejected")
	}
}

func TestCleanupPathRejectsLexicalEscapeResolvedInsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "sibling")
	if err := os.Symlink(root, outside); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if cleanupPath(root, filepath.Join(outside, "child")) {
		t.Fatal("path outside the lexical root was accepted after resolving inside it")
	}
}

func TestCleanupRetentionIsThreeDays(t *testing.T) {
	if cleanupRetention != 3*24*time.Hour {
		t.Fatalf("cleanup retention = %s, want 72h", cleanupRetention)
	}
}

func TestCleanupJobsDoesNotSweepOutsideConfiguredTempRoot(t *testing.T) {
	stateRoot := t.TempDir()
	sweepRoot := t.TempDir()
	outside := filepath.Join(t.TempDir(), "go-build-outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "jobs.test")
	if err := os.WriteFile(marker, []byte("running"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupJobs(stateRoot, sweepRoot, time.Now().Add(365*24*time.Hour), false); err != nil {
		t.Fatalf("cleanupJobs() error = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("entry outside configured temp root was removed: %v", err)
	}
}

func TestCleanupKeepsUnappliedAndSharedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "shared")
	now := cleanupNow()
	cleanupJob(t, root, strings.Repeat("a", 64), "SUCCEEDED", map[string]any{"worktreePath": wt, "repoRoot": repo, "applyState": "applied"})
	cleanupJob(t, root, strings.Repeat("b", 64), "RUNNING", map[string]any{"worktreePath": wt, "repoRoot": repo})
	cleanupJobs(root, t.TempDir(), now, false)
	if _, e := os.Stat(wt); e != nil {
		t.Fatalf("protected shared worktree removed: %v", e)
	}
}

func TestCleanupRemovesAppliedAndFailedWithoutPatchWorktrees(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		extra       map[string]any
	}{{"applied", "SUCCEEDED", map[string]any{"applyState": "applied"}}, {"failed", "FAILED", map[string]any{"discarded": true}}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := cleanupRepo(t, root)
			wt := cleanupWT(t, root, repo, tc.name)
			extra := map[string]any{"worktreePath": wt, "repoRoot": repo}
			for k, v := range tc.extra {
				extra[k] = v
			}
			cleanupJob(t, root, strings.Repeat("c", 64), tc.state, extra)
			cleanupJobs(root, t.TempDir(), cleanupNow(), false)
			if _, e := os.Stat(wt); !os.IsNotExist(e) {
				t.Fatalf("worktree remains or stat failed: %v", e)
			}
		})
	}
}

func TestCleanupDistillsBeforeDeletingExpiredJob(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("d", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"finishedAt": float64(cleanupNow().Unix()) - 8*86400})
	cleanupJobs(root, t.TempDir(), cleanupNow(), true)
	if _, e := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")); !os.IsNotExist(e) {
		t.Fatalf("expired job remains: %v", e)
	}
	b, e := os.ReadFile(filepath.Join(root, "telemetry", "learning_summary.json"))
	if e != nil || !strings.Contains(string(b), id) {
		t.Fatalf("job was not distilled before deletion: %v %s", e, b)
	}
}

func TestCleanupSessionStartKeepsUncheckpointedExpiredJob(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("e", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"finishedAt": float64(cleanupNow().Unix()) - 8*86400,
	})

	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")); err != nil {
		t.Fatalf("uncheckpointed job was removed during session cleanup: %v", err)
	}
}

func TestCleanupPreservesFinalizingEvidence(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("8", 64)
	call := "finalizing-call"
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"call": call, "finishedAt": float64(cleanupNow().Unix()) - 8*86400, "outputFile": id + ".output",
	})
	indexPath := filepath.Join(root, "async_jobs", id+".json")
	index := map[string]any{"jobId": id, "call": call, "call_key": id,
		"project_key": "0123456789abcdef", "stage": "FINALIZING"}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, indexData, 0600); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(root, "pending", id+".json")
	pendingData, err := json.Marshal(map[string]any{"job_id": id, "async_call": call})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, pendingData, 0600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".output")
	if err := os.WriteFile(outputPath, []byte("output"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"), indexPath, pendingPath, outputPath} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("finalizing evidence %s was removed: %v", filepath.Base(path), err)
		}
	}
}

func TestCleanupUsesRecordModTimeAndExistingCheckpoint(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("7", 64)
	record := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
	cleanupJob(t, root, id, "FAILED", nil)
	old := time.Unix(cleanupNow().Unix()-8*86400, 0)
	if err := os.Chtimes(record, old, old); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(root, "telemetry", "learning_summary.json")
	data, err := json.Marshal(map[string]any{"schema_version": 1, "sources": []any{id},
		"records": []any{}, "cohorts": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(summary), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(summary, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("checkpointed stale record remains: %v", err)
	}
}

func TestCleanupRemovesLaunchFileAndSharesActionBound(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < maxJobRecords+1; i++ {
		id := fmt.Sprintf("%064x", i+1)
		cleanupJob(t, root, id, "SUCCEEDED", nil)
		if e := os.WriteFile(filepath.Join(dir, id+".launch"), []byte("launch"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	left := 0
	for i := 0; i < maxJobRecords+1; i++ {
		id := fmt.Sprintf("%064x", i+1)
		if _, e := os.Stat(filepath.Join(dir, id+".launch")); e == nil {
			left++
		}
	}
	if left != 1 {
		t.Fatalf("launch removal action bound left %d, want 1", left)
	}
}

func TestCleanupRemovesOrphanAndEnforcesWorktreeCount(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	orphan := cleanupWT(t, root, repo, "orphan")
	old := time.Unix(cleanupNow().Unix()-8*86400, 0)
	if e := os.Chtimes(orphan, old, old); e != nil {
		t.Fatal(e)
	}
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, e := os.Stat(orphan); !os.IsNotExist(e) {
		t.Fatalf("orphan worktree remains: %v", e)
	}
	root = t.TempDir()
	repo = cleanupRepo(t, root)
	for i := 0; i < maxWorktrees+1; i++ {
		cleanupWT(t, root, repo, fmt.Sprintf("wt-%03d", i))
	}
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	items, e := os.ReadDir(filepath.Join(root, "wt"))
	if e != nil {
		t.Fatal(e)
	}
	if len(items) > maxWorktrees {
		t.Fatalf("worktree count %d exceeds %d", len(items), maxWorktrees)
	}
}

func TestCleanupNeverFollowsLinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if e := os.WriteFile(outside, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	id := strings.Repeat("e", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{"finishedAt": float64(cleanupNow().Unix()) - 8*86400})
	launch := filepath.Join(root, "codex_exec_mcp", "jobs", id+".launch")
	if e := os.Symlink(outside, launch); e != nil {
		t.Skipf("symlink unavailable: %v", e)
	}
	cleanupJobs(root, t.TempDir(), cleanupNow(), true)
	b, e := os.ReadFile(outside)
	if e != nil || string(b) != "keep" {
		t.Fatalf("outside link target changed: %v %q", e, b)
	}
}

func cleanupTelemetryCount(t *testing.T, root, eventType string) int {
	t.Helper()
	count := 0
	_ = filepath.WalkDir(filepath.Join(root, "telemetry", "events"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			var event map[string]any
			if json.Unmarshal([]byte(line), &event) == nil && event["event_type"] == eventType {
				count++
			}
		}
		return nil
	})
	return count
}

func TestCleanupEmitsUnfetchedOnceAndPersistsMarker(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("f", 64)
	call := "unfetched-call"
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"call": call, "finishedAt": float64(cleanupNow().Unix()) - 8*86400,
		"outputFile": id + ".output",
	})
	indexPath := filepath.Join(root, "async_jobs", id+".json")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0700); err != nil {
		t.Fatal(err)
	}
	index := map[string]any{"jobId": id, "call": call, "call_key": id,
		"project_key": "0123456789abcdef", "stage": "PENDING"}
	indexData, _ := json.Marshal(index)
	if err := os.WriteFile(indexPath, indexData, 0600); err != nil {
		t.Fatal(err)
	}
	pendingPath := filepath.Join(root, "pending", id+".json")
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
		t.Fatal(err)
	}
	pendingData, _ := json.Marshal(map[string]any{"job_id": id, "async_call": call, "route": "subtle"})
	if err := os.WriteFile(pendingPath, pendingData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "telemetry"), 0700); err != nil {
		t.Fatal(err)
	}
	summaryData, _ := json.Marshal(map[string]any{"schema_version": 1, "sources": []any{id}, "records": []any{}, "cohorts": map[string]any{}})
	if err := os.WriteFile(filepath.Join(root, "telemetry", "learning_summary.json"), summaryData, 0600); err != nil {
		t.Fatal(err)
	}

	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if got := cleanupTelemetryCount(t, root, "CODEX_RESULT_UNFETCHED"); got != 1 {
		t.Fatalf("unfetched telemetry count = %d, want 1", got)
	}
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if got := cleanupTelemetryCount(t, root, "CODEX_RESULT_UNFETCHED"); got != 1 {
		t.Fatalf("unfetched telemetry count after retry = %d, want 1", got)
	}
}

func TestCleanupClaimFailuresDoNotConsumeActionBound(t *testing.T) {
	root := t.TempDir()
	ids := make([]any, 0, maxJobRecords+1)
	for i := 1; i <= maxJobRecords+1; i++ {
		id := fmt.Sprintf("%064x", i)
		ids = append(ids, id)
		cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
			"finishedAt": float64(cleanupNow().Unix()) - 8*86400,
		})
		if i <= maxJobRecords {
			claim := filepath.Join(root, "async_jobs", id+".claim.lock")
			if err := os.MkdirAll(claim, 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "telemetry"), 0700); err != nil {
		t.Fatal(err)
	}
	summaryData, _ := json.Marshal(map[string]any{"schema_version": 1, "sources": ids, "records": []any{}, "cohorts": map[string]any{}})
	if err := os.WriteFile(filepath.Join(root, "telemetry", "learning_summary.json"), summaryData, 0600); err != nil {
		t.Fatal(err)
	}

	lastID := fmt.Sprintf("%064x", maxJobRecords+1)
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", lastID+".json")); !os.IsNotExist(err) {
		t.Fatalf("successful cleanup after claim failures left last job: %v", err)
	}
}

func TestCleanupMalformedSummaryDoesNotAbort(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("a", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"finishedAt": float64(cleanupNow().Unix()) - 8*86400,
	})
	recentID := strings.Repeat("b", 64)
	cleanupJob(t, root, recentID, "SUCCEEDED", map[string]any{
		"finishedAt": float64(cleanupNow().Unix()),
	})
	launchPath := filepath.Join(root, "codex_exec_mcp", "jobs", recentID+".launch")
	if err := os.WriteFile(launchPath, []byte("launch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "telemetry"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "telemetry", "learning_summary.json"), []byte(`{"schema_version":1,"sources":{},"records":[],"cohorts":{}}`), 0600); err != nil {
		t.Fatal(err)
	}

	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(launchPath); !os.IsNotExist(err) {
		t.Fatalf("malformed summary stopped later cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")); err != nil {
		t.Fatalf("malformed summary changed undistilled job: %v", err)
	}
}

func TestCleanupRemovesOldTerminalUnappliedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "old-unapplied")
	id := strings.Repeat("1", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"worktreePath": wt, "repoRoot": repo,
		"finishedAt": float64(cleanupNow().Unix()) - 8*86400,
	})
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("old terminal worktree remains: %v", err)
	}
}

func TestCleanupKeepsRecentUnappliedAndActiveWorktrees(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		finished    float64
	}{
		{"recent-terminal", "SUCCEEDED", float64(cleanupNow().Unix())},
		{"active", "RUNNING", float64(cleanupNow().Unix()) - 30*86400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := cleanupRepo(t, root)
			wt := cleanupWT(t, root, repo, tc.name)
			cleanupJob(t, root, strings.Repeat("2", 64), tc.state, map[string]any{
				"worktreePath": wt, "repoRoot": repo, "finishedAt": tc.finished,
			})
			cleanupJobs(root, t.TempDir(), cleanupNow(), false)
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("worktree removed while still needed: %v", err)
			}
		})
	}
}

func TestCleanupKeepsLatestReplyThreadWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	oldWT := cleanupWT(t, root, repo, "old-thread")
	newWT := cleanupWT(t, root, repo, "new-thread")
	thread := "thread-reused"
	newID := strings.Repeat("4", 64)
	cleanupJob(t, root, strings.Repeat("3", 64), "SUCCEEDED", map[string]any{
		"worktreePath": oldWT, "repoRoot": repo, "threadId": thread,
		"finishedAt": float64(cleanupNow().Unix()) - 60,
		"applyState": "applied",
	})
	cleanupJob(t, root, newID, "SUCCEEDED", map[string]any{
		"worktreePath": newWT, "repoRoot": repo, "threadId": thread,
		"finishedAt": float64(cleanupNow().Unix()),
		"applyState": "applied",
	})
	prep := filepath.Join(root, "preparations", newID, "revision-1", "state.json")
	if err := os.MkdirAll(filepath.Dir(prep), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prep, []byte(`{"state":"PREPARED"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(oldWT); !os.IsNotExist(err) {
		t.Fatalf("superseded thread worktree remains: %v", err)
	}
	if _, err := os.Stat(newWT); err != nil {
		t.Fatalf("latest reply worktree removed: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(prep)); !os.IsNotExist(err) {
		t.Fatalf("latest thread preparation copy remains: %v", err)
	}
}

func TestCleanupRemovesWorktreeWithReadOnlyFiles(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "readonly")
	file := filepath.Join(wt, "readonly.txt")
	if err := os.WriteFile(file, []byte("readonly"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0400); err != nil {
		t.Fatal(err)
	}
	cleanupJob(t, root, strings.Repeat("5", 64), "SUCCEEDED", map[string]any{
		"worktreePath": wt, "repoRoot": repo, "applyState": "applied",
	})
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("read-only worktree remains: %v", err)
	}
}

func TestCleanupArtifactByteBoundEvictsOldestTerminalFirst(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	oldWT := cleanupWT(t, root, repo, "old-pressure")
	newWT := cleanupWT(t, root, repo, "new-pressure")
	if err := os.WriteFile(filepath.Join(oldWT, "payload"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newWT, "payload"), make([]byte, 16), 0600); err != nil {
		t.Fatal(err)
	}
	oldID, newID := strings.Repeat("6", 64), strings.Repeat("7", 64)
	cleanupJob(t, root, oldID, "SUCCEEDED", map[string]any{"worktreePath": oldWT, "repoRoot": repo, "finishedAt": float64(cleanupNow().Unix()) - 2*86400})
	cleanupJob(t, root, newID, "SUCCEEDED", map[string]any{"worktreePath": newWT, "repoRoot": repo, "finishedAt": float64(cleanupNow().Unix()) - 86400})
	previous := maxArtifactBytes
	newSize := cleanupTreeBytes(newWT) + cleanupTreeBytes(filepath.Join(root, "preparations"))
	maxArtifactBytes = newSize
	t.Cleanup(func() { maxArtifactBytes = previous })
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(oldWT); !os.IsNotExist(err) {
		t.Fatalf("oldest terminal worktree remains above byte bound: %v", err)
	}
	if _, err := os.Stat(newWT); err != nil {
		t.Fatalf("newer terminal worktree was evicted first: %v", err)
	}
}

func TestCleanupArtifactByteBoundKeepsWorktreeUsedByRunningJob(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "active-over-byte-bound")
	if err := os.WriteFile(filepath.Join(wt, "payload"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupJob(t, root, strings.Repeat("a", 64), "SUCCEEDED", map[string]any{
		"worktreePath": wt, "repoRoot": repo, "applyState": "applied",
		"finishedAt": float64(cleanupNow().Unix()) - 2*86400,
	})
	cleanupJob(t, root, strings.Repeat("b", 64), "RUNNING", map[string]any{
		"worktreePath": wt, "repoRoot": repo,
	})
	previous := maxArtifactBytes
	maxArtifactBytes = 0
	t.Cleanup(func() { maxArtifactBytes = previous })
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), false); err != nil {
		t.Fatalf("CleanupJobs() error = %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree used by running job removed over byte bound: %v", err)
	}
}

func TestCleanupArtifactByteBoundCanEvictLatestAppliedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "latest-applied")
	if err := os.WriteFile(filepath.Join(wt, "payload"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 64)
	prepID := strings.Repeat("d", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"worktreePath": wt, "repoRoot": repo, "applyState": "applied", "threadId": "thread",
		"finishedAt": float64(cleanupNow().Unix()),
	})
	prep := filepath.Join(root, "preparations", prepID)
	if err := os.MkdirAll(prep, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prep, "payload"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupJob(t, root, prepID, "SUCCEEDED", map[string]any{
		"worktreePath": wt, "repoRoot": repo, "applyState": "applied", "threadId": "thread-2",
		"finishedAt": float64(cleanupNow().Unix()),
	})
	previous := maxArtifactBytes
	maxArtifactBytes = 0
	t.Cleanup(func() { maxArtifactBytes = previous })
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), false); err != nil {
		t.Fatalf("CleanupJobs() error = %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("latest applied worktree remains above byte bound: %v", err)
	}
	if _, err := os.Stat(prep); !os.IsNotExist(err) {
		t.Fatalf("preparation sharing a worktree candidate was skipped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")); err != nil {
		t.Fatalf("latest job record was removed with its worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", prepID+".json")); err != nil {
		t.Fatalf("latest job record was removed with its preparation: %v", err)
	}
}

func TestCleanupArtifactByteBoundKeepsRecentConflictWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "recent-conflict")
	if err := os.WriteFile(filepath.Join(wt, "payload"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"worktreePath": wt, "repoRoot": repo, "applyState": "CONFLICT",
		"finishedAt": float64(cleanupNow().Unix()),
	})
	previous := maxArtifactBytes
	maxArtifactBytes = 0
	t.Cleanup(func() { maxArtifactBytes = previous })
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), false); err != nil {
		t.Fatalf("CleanupJobs() error = %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("recent conflict worktree removed over byte bound: %v", err)
	}
}

func TestCleanupArtifactByteBoundKeepsRecentUnappliedThreadWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "recent-thread")
	if err := os.WriteFile(filepath.Join(wt, "payload"), make([]byte, 1024), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("f", 64)
	cleanupJob(t, root, id, "SUCCEEDED", map[string]any{
		"worktreePath": wt, "repoRoot": repo, "threadId": "recent-thread",
		"finishedAt": float64(cleanupNow().Unix()),
	})
	previous := maxArtifactBytes
	maxArtifactBytes = 0
	t.Cleanup(func() { maxArtifactBytes = previous })
	if err := cleanupJobs(root, t.TempDir(), cleanupNow(), false); err != nil {
		t.Fatalf("CleanupJobs() error = %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("recent unapplied thread worktree removed over byte bound: %v", err)
	}
}

func TestCleanupRemovesOldTimedOutUnappliedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "timed-out")
	id := strings.Repeat("8", 64)
	cleanupJob(t, root, id, "TIMED_OUT", map[string]any{
		"worktreePath": wt, "repoRoot": repo,
		"finishedAt": float64(cleanupNow().Unix()) - 8*86400,
	})
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("old timed-out worktree remains: %v", err)
	}
}

func TestCleanupKeepsUnknownWorktreeOverArtifactByteCap(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "unknown")
	if err := os.WriteFile(filepath.Join(wt, "payload"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("9", 64)
	cleanupJob(t, root, id, "UNKNOWN_OR_IN_PROGRESS", map[string]any{
		"worktreePath": wt, "repoRoot": repo,
	})
	previous := maxArtifactBytes
	maxArtifactBytes = 0
	t.Cleanup(func() { maxArtifactBytes = previous })
	cleanupJobs(root, t.TempDir(), cleanupNow(), false)
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("unknown worktree was removed over artifact byte cap: %v", err)
	}
}
