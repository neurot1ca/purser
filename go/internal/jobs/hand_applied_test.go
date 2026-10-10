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

	"purser-hook/internal/contextio"
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
		{name: "patch-unavailable job applied by hand", state: "FAILED", matching: true, wantOld: "RELEASED", wantNext: "ACTIVE"},
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
			if tc.state == "FAILED" {
				job["errorCode"] = "PATCH_UNAVAILABLE"
			}
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

func TestHandAppliedPlanJobCompletesAndAdvancesMilestone(t *testing.T) {
	root, repo, worktree, snapshot := handAppliedAdmissionFixture(t, true)
	artifact := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(artifact, []byte("accepted plan"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := contextio.AcceptPlan(root, repo, "plan-hand-applied", artifact, []int{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	taskID := "plan-hand-applied-m1of2"
	jobID := strings.Repeat("a", 64)
	if _, matched, err := contextio.BindPlanJob(root, repo, taskID, jobID); err != nil || !matched {
		t.Fatalf("bind plan job: matched=%v err=%v", matched, err)
	}
	job := map[string]any{"jobId": jobID, "state": "SUCCEEDED", "worktreePath": worktree, "repoRoot": repo, "snapshot": snapshot, "taskId": taskID}
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
	if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	writeHandAppliedJSON(t, jobPath, job)
	markHandAppliedJob(root, jobID)
	updated, err := contextio.ReadPlanForTask(root, repo, plan.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentMilestone != 2 || len(updated.CompletedMilestones) != 1 || updated.CompletedMilestones[0] != 1 || len(updated.AppliedPasses) != 1 || len(updated.LinkedJobs) != 0 {
		t.Fatalf("hand-applied plan progress = %+v", updated)
	}
}

func TestAppliedJobSupersedesEarlierSucceededTaskJob(t *testing.T) {
	root := t.TempDir()
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatal(err)
	}
	priorID, appliedID := strings.Repeat("1", 64), strings.Repeat("2", 64)
	project := strings.Repeat("c", 16)
	owner := strings.Repeat("d", 64)
	projectRoot := filepath.Join(root, "projects", project)
	store := lease.New(projectRoot)
	if got := store.Acquire(owner, "same-task", []string{"src"}, lease.AcquireOptions{JobID: &priorID}, 100); got.Status != "OK" {
		t.Fatalf("seed prior scope lease: %#v", got)
	}
	if err := os.MkdirAll(filepath.Join(root, "async_jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	writeHandAppliedJSON(t, filepath.Join(root, "async_jobs", priorID+".json"), map[string]any{"jobId": priorID, "project_key": project, "call_key": owner})
	prior := map[string]any{"jobId": priorID, "state": "SUCCEEDED", "taskId": "same-task", "threadId": "same-thread", "acceptedAt": float64(1)}
	current := map[string]any{"jobId": appliedID, "state": "SUCCEEDED", "taskId": "same-task", "threadId": "same-thread", "acceptedAt": float64(2), "applyState": "applied"}
	writeHandAppliedJSON(t, filepath.Join(jobsDir, priorID+".json"), prior)
	writeHandAppliedJSON(t, filepath.Join(jobsDir, appliedID+".json"), current)

	supersedeEarlierTaskJobs(root, appliedID, current)

	got, err := Read(filepath.Join(jobsDir, priorID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var state, supersededBy string
	got.Get("state", &state)
	got.Get("supersededBy", &supersededBy)
	if state != "SUPERSEDED" || supersededBy != appliedID {
		t.Fatalf("prior state=%q supersededBy=%q, want SUPERSEDED by applied job", state, supersededBy)
	}
	leaseState, err := store.OwnerState(owner, 101)
	if err != nil || leaseState != "RELEASED" {
		t.Fatalf("prior lease state=%q, %v; want RELEASED", leaseState, err)
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
