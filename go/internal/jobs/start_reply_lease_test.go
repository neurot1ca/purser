package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/lease"
)

func TestReplyAdmissionReleasesOnlyTerminalUnappliedJobLease(t *testing.T) {
	for _, state := range []string{"SUCCEEDED", "RUNNING"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			repo := t.TempDir()
			gitStart(t, repo, "init", "-q")
			gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "base")
			worktree := filepath.Join(t.TempDir(), "reply-worktree")
			if state == "SUCCEEDED" {
				gitStart(t, repo, "worktree", "add", "-qb", "reply", worktree, "HEAD")
			}
			priorID := strings.Repeat("a", 64)
			owner := strings.Repeat("b", 64)
			project := strings.Repeat("c", 16)
			asyncRoot := filepath.Join(root, "projects", project)
			idCopy := priorID
			if got := lease.New(asyncRoot).Acquire(owner, "reply-task", []string{"src"}, lease.AcquireOptions{JobID: &idCopy}, float64(time.Now().Unix())); got.Status != "OK" {
				t.Fatalf("seed previous lease: %#v", got)
			}
			if err := os.MkdirAll(filepath.Join(root, "async_jobs"), 0700); err != nil {
				t.Fatal(err)
			}
			index, _ := json.Marshal(map[string]any{"jobId": priorID, "project_key": project, "call_key": owner})
			if err := os.WriteFile(filepath.Join(root, "async_jobs", priorID+".json"), index, 0600); err != nil {
				t.Fatal(err)
			}
			prior := map[string]any{"jobId": priorID, "state": state, "threadId": "reply-thread", "taskId": "reply-task", "repoRoot": repo}
			thread := map[string]any{"thread_id": "reply-thread", "repoRoot": repo, "cwd": repo, "config_args": []any{}}
			if state == "SUCCEEDED" {
				prior["worktreePath"], prior["finishedAt"] = worktree, float64(time.Now().UnixNano())/1e9
				thread["worktreePath"], thread["cwd"] = worktree, worktree
			}
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			writeHostTestJSON(t, filepath.Join(dir, priorID+".json"), prior)
			if err := SaveRecord(filepath.Join(root, "codex_exec_mcp", "threads"), thread); err != nil {
				t.Fatal(err)
			}
			_, _, err := Start(root, map[string]any{"call": "00000000-0000-4000-8000-000000000001", "prompt": "continue", "mode": "reply", "threadId": "reply-thread"}, StartOptions{
				StateRoot: root, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			leaseState, err := lease.New(asyncRoot).OwnerState(owner, float64(time.Now().Unix()))
			if err != nil {
				t.Fatal(err)
			}
			want := "ACTIVE"
			if state == "SUCCEEDED" {
				want = "RELEASED"
			}
			if leaseState != want {
				t.Fatalf("previous owner lease state = %q, want %q", leaseState, want)
			}
		})
	}
}
