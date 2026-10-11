package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"purser-hook/internal/retention"
)

func TestUnappliedThreadJobFallsBackToAcceptedAt(t *testing.T) {
	jobsDir := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	for _, value := range []map[string]any{
		{"jobId": "earlier", "threadId": "thread", "state": "SUCCEEDED", "applyState": "pending", "worktreePath": worktree, "acceptedAt": 1.0, "finishedAt": 2.0},
		{"jobId": "later", "threadId": "thread", "state": "SUCCEEDED", "applyState": "pending", "worktreePath": worktree, "acceptedAt": 3.0},
	} {
		writeHostTestJSON(t, filepath.Join(jobsDir, value["jobId"].(string)+".json"), value)
	}
	if got := nonemptyString(recordMap(unappliedThreadJob(jobsDir, "thread"))["jobId"]); got != "later" {
		t.Fatalf("latest unapplied job = %q, want later", got)
	}
}

func TestStartReplyUsesFreshWorktreeWhenThreadWorktreeIsRetiring(t *testing.T) {
	repo := t.TempDir()
	gitStart(t, repo, "init", "-q")
	gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "base")
	worktree := filepath.Join(t.TempDir(), "retiring-worktree")
	gitStart(t, repo, "worktree", "add", "-qb", "reply", worktree, "HEAD")
	state := t.TempDir()
	if _, err := retention.MarkWorktreeRetiring(state, worktree); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecord(filepath.Join(state, "codex_exec_mcp", "threads"), map[string]any{
		"thread_id": "retiring-thread", "cwd": worktree, "repoRoot": repo, "worktreePath": worktree, "config_args": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	var launched bool
	opt := StartOptions{StateRoot: state, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true},
		Launch: func(id string, _ int64, _ string) error {
			launched = true
			launchBytes, err := os.ReadFile(filepath.Join(state, "codex_exec_mcp", "jobs", id+".launch"))
			if err != nil {
				return err
			}
			var launch struct {
				Args map[string]any `json:"args"`
			}
			if err := json.Unmarshal(launchBytes, &launch); err != nil {
				return err
			}
			if launch.Args["worktree"] != true || launch.Args["cwd"] != repo {
				t.Errorf("retiring reply launch args = %#v", launch.Args)
			}
			return nil
		}, Await: func(id string, _ *Record) map[string]any {
			return jobResult(map[string]any{"jobId": id, "state": "QUEUED"})
		}}
	if _, _, err := Start(state, map[string]any{"call": "00000000-0000-0000-0000-000000000001", "prompt": "continue", "mode": "reply", "threadId": "retiring-thread"}, opt); err != nil {
		t.Fatal(err)
	}
	if !launched {
		t.Fatal("reply host was not launched")
	}
}

func TestRunHostGenerationMapsRequestedWorktreeCWD(t *testing.T) {
	for _, tc := range []struct {
		name, requested string
		createNested    bool
		fallback        bool
	}{
		{name: "missing subdirectory falls back to worktree", requested: filepath.Join(".tmp", "opencode-probe"), fallback: true},
		{name: "existing relative subdirectory is preserved", requested: "nested", createNested: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			gitStart(t, repo, "init", "-q")
			if tc.fallback {
				if err := os.MkdirAll(filepath.Join(repo, tc.requested), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".tmp/\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitStart(t, repo, "add", ".gitignore")
				gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "ignore scratch")
			}
			if tc.createNested {
				if err := os.MkdirAll(filepath.Join(repo, tc.requested), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, tc.requested, "tracked.txt"), []byte("nested\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitStart(t, repo, "add", tc.requested)
				gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "nested")
			}
			state := t.TempDir()
			dir := filepath.Join(state, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			requested := tc.requested
			if filepath.IsAbs(requested) == false {
				requested = filepath.Join(repo, requested)
			}
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
				"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
				"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
			})
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
				"name": "codex", "agent": "codex", "args": map[string]any{"cwd": requested, "prompt": "work", "worktree": true,
					"config": map[string]any{"sandbox_workspace_write": map[string]any{"writable_roots": []any{repo}}}},
			})
			t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
			t.Setenv("PURSER_HOST_FAKE_MODE", "")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			var wantCWD string
			_, err = RunHostGeneration(dir, hostTestID, 1, "owner", HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
				worktree := nonemptyString(recordMap(readJob(dir, hostTestID))["worktreePath"])
				if tc.fallback {
					wantCWD = worktree
				} else {
					wantCWD = filepath.Join(worktree, tc.requested)
				}
				if filepath.Clean(args.Cwd) != filepath.Clean(wantCWD) {
					t.Errorf("worker cwd = %q, want %q", args.Cwd, wantCWD)
				}
				return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			job := recordMap(readJob(dir, hostTestID))
			if tc.fallback && job["cwdFallback"] != filepath.FromSlash(tc.requested) {
				t.Fatalf("cwdFallback = %#v, want %q", job["cwdFallback"], tc.requested)
			}
			if !tc.fallback {
				if _, ok := job["cwdFallback"]; ok {
					t.Fatalf("existing requested cwd was marked as fallback: %#v", job["cwdFallback"])
				}
			}
		})
	}
}
