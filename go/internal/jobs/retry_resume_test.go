package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/ledger"
)

func TestHostFailedRetryResume(t *testing.T) {
	for _, mode := range []string{"resume", "oversized", "write failure", "read failure", "apply failure", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			root := jobsScratchDir(t, "retry-resume-")
			repo := filepath.Join(root, "repo with spaces")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "init", "-q")
			git(t, repo, "config", "user.email", "test@example.com")
			git(t, repo, "config", "user.name", "test")
			git(t, repo, "config", "core.autocrlf", "false")
			if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("before\r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "add", ".")
			git(t, repo, "commit", "-qm", "initial")
			state := filepath.Join(root, "state")
			dir := filepath.Join(state, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			snapshot, err := ledger.CreateJobWorktree(repo, hostTestID, filepath.Join(state, "wt"))
			if err != nil {
				t.Fatal(err)
			}
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner", "taskId": "retry-task", "repoRoot": repo, "worktreePath": snapshot["path"], "snapshot": snapshot["snapshot"]})
			if err := persistTaskAttempt(state, recordMap(readJob(dir, hostTestID)), repo); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(snapshot["path"], "tracked.txt"), []byte("partial\r\n"), 0600); err != nil {
				t.Fatal(err)
			}
			data := []byte{0, 1, 2, 13, 10}
			if mode == "oversized" {
				data = []byte(strings.Repeat("x", 5<<20))
			}
			if err := os.WriteFile(filepath.Join(snapshot["path"], "new file.bin"), data, 0600); err != nil {
				t.Fatal(err)
			}
			patch := filepath.Join(dir, hostTestID+".partial.patch")
			if mode == "write failure" {
				if err := os.Mkdir(patch, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unavailable" {
				_, err = failHostUnavailable(filepath.Join(dir, hostTestID+".json"), hostTestID, 1, "owner", "unavailable")
			} else {
				_, err = finishHostError(filepath.Join(dir, hostTestID+".json"), hostTestID, 1, "owner", "CODEX_FAILED", "usage limit")
			}
			if err != nil {
				t.Fatal(err)
			}
			attempt, _, err := enforce.LoadTaskAttempt(state, contextio.ProjectKey(repo), "retry-task")
			if err != nil || attempt.TerminalDisposition != "failed" {
				t.Fatalf("attempt = %#v, %v", attempt, err)
			}
			if mode == "read failure" {
				if err := os.Remove(patch); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(patch, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "apply failure" {
				if err := os.WriteFile(patch, []byte("invalid patch"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
			t.Setenv("PURSER_HOST_FAKE_MODE", "stdin")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			_, retry, err := Start(state, map[string]any{"call": "00000000-0000-4000-8000-000000000123", "cwd": repo, "taskId": "retry-task", "prompt": "continue", "worktree": true}, StartOptions{Validation: StartValidationOptions{Agent: "codex", FixedAgent: true, ValidateAgent: func(string) error { return nil }}, Launch: func(id string, generation int64, token string) error {
				_, err := RunHostGeneration(dir, id, generation, token, HostOptions{StateRoot: state, WorkerCommand: func(args LaunchArgs) ([]string, error) {
					return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
				}})
				return err
			}})
			if err != nil || retry == nil {
				t.Fatalf("retry: %v, %v", retry, err)
			}
			m := recordMap(readJob(dir, nonemptyString(recordMap(retry)["jobId"])))
			if m["state"] != "SUCCEEDED" {
				t.Fatalf("retry = %#v", m)
			}
			prompt, err := os.ReadFile(filepath.Join(dir, nonemptyString(m["outputFile"])))
			if err != nil {
				t.Fatal(err)
			}
			resumed := mode == "resume" || mode == "unavailable"
			want := "before\r\n"
			if resumed {
				want = "partial\r\n"
			}
			got, err := os.ReadFile(filepath.Join(nonemptyString(m["worktreePath"]), "tracked.txt"))
			if err != nil || string(got) != want {
				t.Fatalf("tracked = %q, %v", got, err)
			}
			line := "Partial changes from an interrupted earlier attempt are already applied in this worktree; review and continue from them."
			count := 0
			if resumed {
				count = 1
				got, err = os.ReadFile(filepath.Join(nonemptyString(m["worktreePath"]), "new file.bin"))
				if err != nil || string(got) != string(data) {
					t.Fatalf("untracked = %q, %v", got, err)
				}
			} else if _, err := os.Stat(filepath.Join(nonemptyString(m["worktreePath"]), "new file.bin")); !os.IsNotExist(err) {
				t.Fatalf("failed resume left an untracked file: %v", err)
			}
			if strings.Count(string(prompt), line) != count {
				t.Fatalf("prompt = %q", prompt)
			}
			if !resumed && nonemptyString(m["resumeStatus"]) == "" {
				t.Fatalf("missing skip reason: %#v", m)
			}
			if resumed {
				if _, err := os.Stat(patch); !os.IsNotExist(err) {
					t.Fatalf("consumed patch remains: %v", err)
				}
			}
		})
	}
}

func TestHostResumeAttemptOwnershipAndTaskFinish(t *testing.T) {
	root := jobsScratchDir(t, "retry-finish-")
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(dir, hostTestID+".partial.patch")
	if err := os.WriteFile(patch, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	attempt := enforce.TaskAttempt{ProjectKey: "0123456789abcdef", TaskID: "task", LatestJobID: hostTestID, TerminalDisposition: "started"}
	if err := enforce.SaveTaskAttempt(root, attempt); err != nil {
		t.Fatal(err)
	}
	stale := attempt
	stale.LatestJobID = strings.Repeat("a", 64)
	stale.TerminalDisposition = "failed"
	if err := enforce.SaveTaskAttempt(root, stale); err != nil {
		t.Fatal(err)
	}
	got, _, err := enforce.LoadTaskAttempt(root, attempt.ProjectKey, attempt.TaskID)
	if err != nil || got.LatestJobID != hostTestID || got.TerminalDisposition != "started" {
		t.Fatalf("late finish overwrote attempt: %#v, %v", got, err)
	}
	if err := enforce.SaveTaskAttempt(root, enforce.TaskAttempt{ProjectKey: attempt.ProjectKey, TaskID: attempt.TaskID, TerminalDisposition: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(patch); !os.IsNotExist(err) {
		t.Fatalf("finished task patch remains: %v", err)
	}
}
