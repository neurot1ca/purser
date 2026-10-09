package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/lease"
)

func TestAdmissionReleasesOnlyHandAppliedSucceededBlockers(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		matching    bool
		wantOld     string
		wantNext    string
	}{
		{name: "matching hand-applied owner", state: "SUCCEEDED", matching: true, wantOld: "RELEASED", wantNext: "ACTIVE"},
		{name: "differing file", state: "SUCCEEDED", wantOld: "ACTIVE", wantNext: "QUEUED"},
		{name: "running owner", state: "RUNNING", matching: true, wantOld: "ACTIVE", wantNext: "QUEUED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repo, worktree, snapshot := handAppliedAdmissionFixture(t, tc.matching)
			projectKey := strings.Repeat("c", 16)
			projectRoot := filepath.Join(root, "projects", projectKey)
			now := float64(time.Now().Unix())
			call := "prior-hand-applied-call"
			sum := sha256.Sum256([]byte(call))
			jobID := hex.EncodeToString(sum[:])
			ownerKey := strings.Repeat("b", 64)
			store := lease.New(projectRoot)
			if got := store.Acquire(ownerKey, "prior-task", []string{"src"}, lease.AcquireOptions{JobID: &jobID}, now); got.Status != "OK" {
				t.Fatalf("seed blocker lease: %#v", got)
			}
			jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(jobsDir, 0700); err != nil {
				t.Fatal(err)
			}
			job := map[string]any{"jobId": jobID, "call": call, "state": tc.state, "worktreePath": worktree, "repoRoot": repo, "snapshot": snapshot, "taskId": "prior-task"}
			writeHandAppliedJSON(t, filepath.Join(jobsDir, jobID+".json"), job)
			indexDir := filepath.Join(root, "async_jobs")
			if err := os.MkdirAll(indexDir, 0700); err != nil {
				t.Fatal(err)
			}
			writeHandAppliedJSON(t, filepath.Join(indexDir, jobID+".json"), map[string]any{"jobId": jobID, "project_key": projectKey, "call_key": ownerKey})

			ReleaseHandAppliedBlockers(root, projectRoot, []string{"src"})
			nextID := strings.Repeat("d", 64)
			got := store.Acquire(strings.Repeat("e", 64), "next-task", []string{"src"}, lease.AcquireOptions{JobID: &nextID, QueueConflict: true}, now+1)
			if got.Status != "OK" || (got.Code == "LEASE_QUEUED") != (tc.wantNext == "QUEUED") {
				t.Fatalf("next admission = %#v, want state %s", got, tc.wantNext)
			}
			state, err := store.OwnerState(ownerKey, now+2)
			if err != nil || state != tc.wantOld {
				t.Fatalf("blocking lease state = %q, %v; want %q", state, err, tc.wantOld)
			}
			var saved map[string]any
			data, err := os.ReadFile(filepath.Join(jobsDir, jobID+".json"))
			if err != nil || json.Unmarshal(data, &saved) != nil {
				t.Fatalf("hand-applied job record unreadable: %#v (%v)", saved, err)
			}
			if tc.wantOld == "RELEASED" {
				if saved["applyState"] != "applied" || saved["applyReason"] != "hand_applied_detected" || saved["decisionRecorded"] != nil {
					t.Fatalf("hand-applied job was not recorded without a decision: %#v", saved)
				}
			} else if saved["applyState"] == "applied" {
				t.Fatalf("blocking job was incorrectly marked applied: %#v", saved)
			}
		})
	}
}

func handAppliedAdmissionFixture(t *testing.T, matching bool) (root, repo, worktree, snapshot string) {
	t.Helper()
	root, repo = t.TempDir(), filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "file.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	handAppliedGit(t, repo, "init", "-q")
	handAppliedGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "add", "src/file.txt")
	handAppliedGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "base")
	snapshot = strings.TrimSpace(handAppliedGit(t, repo, "rev-parse", "HEAD"))
	worktree = filepath.Join(t.TempDir(), "job-worktree")
	handAppliedGit(t, repo, "worktree", "add", "-qb", "job-branch", worktree, snapshot)
	if err := os.WriteFile(filepath.Join(worktree, "src", "file.txt"), []byte("hand-applied\n"), 0600); err != nil {
		t.Fatal(err)
	}
	content := "different\n"
	if matching {
		content = "hand-applied\n"
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "file.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return root, repo, worktree, snapshot
}

func handAppliedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func writeHandAppliedJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
