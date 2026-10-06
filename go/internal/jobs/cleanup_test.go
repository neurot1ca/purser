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

func TestCleanupKeepsUnappliedAndSharedWorktree(t *testing.T) {
	root := t.TempDir()
	repo := cleanupRepo(t, root)
	wt := cleanupWT(t, root, repo, "shared")
	now := cleanupNow()
	cleanupJob(t, root, strings.Repeat("a", 64), "SUCCEEDED", map[string]any{"worktreePath": wt, "repoRoot": repo, "applyState": "applied"})
	cleanupJob(t, root, strings.Repeat("b", 64), "RUNNING", map[string]any{"worktreePath": wt, "repoRoot": repo})
	CleanupJobs(root, now, false)
	if _, e := os.Stat(wt); e != nil {
		t.Fatalf("protected shared worktree removed: %v", e)
	}
}

func TestCleanupRemovesAppliedAndFailedWithoutPatchWorktrees(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		extra       map[string]any
	}{{"applied", "SUCCEEDED", map[string]any{"applyState": "applied"}}, {"failed", "FAILED", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := cleanupRepo(t, root)
			wt := cleanupWT(t, root, repo, tc.name)
			extra := map[string]any{"worktreePath": wt, "repoRoot": repo}
			for k, v := range tc.extra {
				extra[k] = v
			}
			cleanupJob(t, root, strings.Repeat("c", 64), tc.state, extra)
			CleanupJobs(root, cleanupNow(), false)
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
	CleanupJobs(root, cleanupNow(), true)
	if _, e := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")); !os.IsNotExist(e) {
		t.Fatalf("expired job remains: %v", e)
	}
	b, e := os.ReadFile(filepath.Join(root, "telemetry", "learning_summary.json"))
	if e != nil || !strings.Contains(string(b), id) {
		t.Fatalf("job was not distilled before deletion: %v %s", e, b)
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
	CleanupJobs(root, cleanupNow(), false)
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
	CleanupJobs(root, cleanupNow(), false)
	if _, e := os.Stat(orphan); !os.IsNotExist(e) {
		t.Fatalf("orphan worktree remains: %v", e)
	}
	root = t.TempDir()
	repo = cleanupRepo(t, root)
	for i := 0; i < maxWorktrees+1; i++ {
		cleanupWT(t, root, repo, fmt.Sprintf("wt-%03d", i))
	}
	CleanupJobs(root, cleanupNow(), false)
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
	CleanupJobs(root, cleanupNow(), true)
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

	CleanupJobs(root, cleanupNow(), false)
	if got := cleanupTelemetryCount(t, root, "CODEX_RESULT_UNFETCHED"); got != 1 {
		t.Fatalf("unfetched telemetry count = %d, want 1", got)
	}
	CleanupJobs(root, cleanupNow(), false)
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
	CleanupJobs(root, cleanupNow(), false)
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

	CleanupJobs(root, cleanupNow(), false)
	if _, err := os.Stat(launchPath); !os.IsNotExist(err) {
		t.Fatalf("malformed summary stopped later cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")); err != nil {
		t.Fatalf("malformed summary changed undistilled job: %v", err)
	}
}
