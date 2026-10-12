package jobs

import "purser-hook/internal/testutil"

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
	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
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
		{name: "differing file", state: "SUCCEEDED", wantOld: "RELEASED", wantNext: "ACTIVE"},
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
			writeHandAppliedJSON(t, filepath.Join(indexDir, jobID+".json"), map[string]any{"jobId": jobID, "project_key": projectKey, "call_key": ownerKey, "call": call})

			// A committed manual apply must be recognised on admission or board refresh even
			// after admission reconciliation has already released the lease.
			if tc.matching && tc.state != "RUNNING" {
				handAppliedGit(t, repo, "add", "src/file.txt")
				handAppliedGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "manual apply")
			}
			if _, err := store.Reconcile(now + 1); err != nil {
				t.Fatal(err)
			}
			if tc.state == "SUCCEEDED" && tc.matching {
				ReleaseHandAppliedBlockers(root, projectRoot, []string{"src"})
			} else {
				enforce.PostProjectJobBoard(root, projectKey, time.Now())
			}
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
			if tc.matching && tc.state != "RUNNING" {
				if saved["applyState"] != "applied" || saved["applyReason"] != "hand_applied_detected" || saved["decisionRecorded"] != true {
					t.Fatalf("hand-applied job was not marked decision-recorded: %#v", saved)
				}
				eventsPath := filepath.Join(root, "telemetry", "events", "by_task", "prior-task.jsonl")
				data, err := os.ReadFile(eventsPath)
				if err != nil || !strings.Contains(string(data), `"event_type":"CLAUDE_DECISION_RECORDED"`) || !strings.Contains(string(data), `"job_id":"`+jobID+`"`) {
					t.Fatalf("hand-applied decision outcome missing job attribution: %s (%v)", data, err)
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

func TestHandAppliedDetectionIgnoresMainOnlyWorktreePaths(t *testing.T) {
	root, repo, worktree, snapshot := handAppliedAdmissionFixture(t, true)
	jobID := strings.Repeat("9", 64)
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatal(err)
	}
	prep := filepath.Join(root, "preparations", jobID)
	workerTree := filepath.Join(prep, "worker-tree")
	if err := os.MkdirAll(workerTree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "worker-test.go"), []byte("package worker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workerPatch, err := ledger.JobPatch(worktree, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workerTree, "worker-test.go"), []byte("package worker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prep, "worker.patch"), []byte(workerPatch), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prep, "state.json"), []byte(`{"state":"PREPARING"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Model the partially replayed worktree: it now contains only the main checkout's paths.
	if err := os.Remove(filepath.Join(worktree, "src", "file.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(worktree, "worker-test.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "main-only.txt"), []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main-only.txt"), []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"jobId": jobID, "state": "SUCCEEDED", "worktreePath": worktree, "repoRoot": repo, "snapshot": snapshot}
	writeHandAppliedJSON(t, filepath.Join(jobsDir, jobID+".json"), job)

	markHandAppliedJob(root, jobID)

	saved, err := Read(filepath.Join(jobsDir, jobID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var applyState string
	saved.Get("applyState", &applyState)
	if applyState == "applied" {
		t.Fatal("main-only worktree paths were incorrectly accepted as the worker patch")
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
	cmd := testutil.Hide(exec.Command("git", args...))
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
