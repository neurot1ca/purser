package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/policy"
	"purser-hook/internal/retention"
)

const hostTestID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHostFakeWorker(t *testing.T) {
	if os.Getenv("PURSER_HOST_FAKE_WORKER") != "1" {
		return
	}
	output := os.Args[len(os.Args)-1]
	mode := os.Getenv("PURSER_HOST_FAKE_MODE")
	if mode == "leaf" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	if mode == "tree" || mode == "tree-wait" {
		child := exec.Command(os.Args[0], "-test.run=TestHostFakeWorker", "--", output)
		child.Env = replaceValidationEnv(os.Environ(), "PURSER_HOST_FAKE_MODE", "leaf")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(child.Process.Pid)
		if err := os.WriteFile(os.Getenv("PURSER_HOST_TREE_PID"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if mode == "tree-wait" {
			time.Sleep(time.Hour)
		}
	}
	if mode == "stdin" {
		prompt, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString("{\"type\": \"thread.started\", \"thread_id\": \"fake-thread\"}\r\n")
		_, _ = os.Stdout.WriteString("{\"type\": \"turn.completed\"}\r\n")
		if err := os.WriteFile(output, prompt, 0600); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	if os.Getenv("PURSER_HOST_FAKE_AGENT") == "opencode" {
		_, _ = os.Stdout.WriteString("{\"type\": \"step_start\", \"sessionID\": \"fake-session\"}\r\n")
		if mode == "agent_failure" {
			_, _ = os.Stdout.WriteString("{\"type\": \"error\", \"message\": \"fake agent failure\"}\r\n")
			os.Exit(1)
		}
		_, _ = os.Stdout.WriteString("{\"type\": \"text\", \"sessionID\": \"fake-session\", \"part\": {\"text\": \"fake answer\"}}\r\n")
		_, _ = os.Stdout.WriteString("{\"type\": \"step_finish\", \"sessionID\": \"fake-session\", \"part\": {\"reason\": \"stop\"}}\r\n")
		os.Exit(0)
	}
	if mode == "sleep" {
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("{\"type\": \"thread.started\", \"thread_id\": \"fake-thread\"}\r\n")
	_, _ = os.Stdout.WriteString("{\"type\": \"item.completed\", \"item\": {\"type\": \"agent_message\", \"text\": \"fake answer\"}}\r\n")
	if os.Getenv("PURSER_HOST_FAKE_TEST_EVIDENCE") == "1" {
		_, _ = os.Stdout.WriteString("{\"type\": \"item.completed\", \"item\": {\"type\": \"command_execution\", \"command\": \"go test ./...\", \"exit_code\": 0, \"aggregated_output\": \"ok\\n\"}}\r\n")
	}
	_, _ = os.Stdout.WriteString("{\"type\": \"turn.completed\"}\r\n")
	result := []byte("fake answer")
	if mode == "edit" {
		if err := os.WriteFile("reply.txt", []byte("reply edit"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "environment" {
		result, _ = json.Marshal(map[string]string{
			"GOCACHE": os.Getenv("GOCACHE"), "GOTMPDIR": os.Getenv("GOTMPDIR"),
			"TEMP": os.Getenv("TEMP"), "TMP": os.Getenv("TMP"), "TMPDIR": os.Getenv("TMPDIR"),
		})
		_ = os.WriteFile(os.Getenv("PURSER_HOST_FAKE_ENV_CAPTURE"), result, 0600)
		result = []byte("fake answer")
	}
	_ = os.WriteFile(output, result, 0600)
	if mode == "nonzero" {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestRunHostGenerationPreservesPromptBytesOnStdin(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	prompt := "preserve exact UTF-8: café 雪\n"
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex", "agent": "codex", "args": map[string]any{"cwd": root, "prompt": prompt},
	})
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "stdin")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunHostGeneration(dir, hostTestID, 1, "owner", HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	job := readJob(dir, hostTestID)
	var output string
	if !job.Get("outputFile", &output) {
		t.Fatal("completed job has no output file")
	}
	got, err := os.ReadFile(filepath.Join(dir, output))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != prompt {
		t.Fatalf("worker stdin bytes = %q, want %q", got, prompt)
	}
}

func TestFinishedJobThreadRecordAdmitsReply(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"jobId": hostTestID, "state": "QUEUED", "generation": 1,
		"ownershipToken": "owner", "signalToken": "signal", "deadlineAt": float64(time.Now().Add(10*time.Second).UnixNano()) / 1e9,
		"taskId": "task-finished", "route": "normal", "model": "gpt-test", "effort": "medium"}
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), job)
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex", "agent": "codex", "args": map[string]any{"cwd": root, "prompt": "hello"},
	})
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options := HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}}
	if _, err = RunHostGeneration(dir, hostTestID, 1, "owner", options); err != nil {
		t.Fatal(err)
	}
	thread := LoadRecord(filepath.Join(root, "codex_exec_mcp", "threads"), "fake-thread")
	requested := map[string]any{"task_id": "task-finished", "route": "normal", "model": "gpt-test", "effort": "medium"}
	if decision := policy.ThreadCompatibility(requested, thread); !decision.Compatible {
		t.Fatalf("reply compatibility = %#v, want admitted thread record %#v", decision, thread)
	}
}

func TestIncompleteFinishedJobThreadIdentityRefusesReply(t *testing.T) {
	requested := map[string]any{"task_id": "task-finished", "route": "normal", "model": "gpt-test", "effort": "medium"}
	recorded := map[string]any{"task_id": "task-finished", "model": "gpt-test"}
	if decision := policy.ThreadCompatibility(requested, recorded); decision.Compatible || decision.Code != "INVALID_LIVE_THREAD" {
		t.Fatalf("incomplete reply identity decision = %#v, want INVALID_LIVE_THREAD", decision)
	}
}

func TestRunHostGenerationWorkerCases(t *testing.T) {
	tests := []struct {
		name, mode, state, errorCode string
		deadline                     time.Duration
		cancel                       bool
		stale                        bool
	}{
		{name: "success", state: "SUCCEEDED"},
		{name: "worker_nonzero", mode: "nonzero", state: "FAILED", errorCode: "CODEX_FAILED"},
		{name: "worker_launch_failure", state: "FAILED", errorCode: "CODEX_FAILED"},
		{name: "deadline", mode: "sleep", state: "TIMED_OUT", errorCode: "DEADLINE_EXCEEDED", deadline: 120 * time.Millisecond},
		{name: "deadline_before_worker_start", state: "TIMED_OUT", errorCode: "DEADLINE_EXCEEDED", deadline: -time.Second},
		{name: "signal_cancel", mode: "sleep", state: "CANCELLED", errorCode: "CANCELLED", deadline: 5 * time.Second, cancel: true},
		{name: "stale_generation", state: "QUEUED", stale: true},
		{name: "worktree_setup_unavailable", state: "FAILED", errorCode: "WORKTREE_MISSING"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "success" {
				t.Setenv("PURSER_HOST_FAKE_TEST_EVIDENCE", "1")
			}
			root := t.TempDir()
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			if tc.deadline != 0 {
				deadline = time.Now().Add(tc.deadline)
			}
			job := map[string]any{"jobId": hostTestID, "state": "QUEUED", "generation": 1,
				"ownershipToken": "owner", "signalToken": "signal", "deadlineAt": float64(deadline.UnixNano()) / 1e9}
			if tc.stale {
				job["generation"] = 2
			}
			if tc.name == "worktree_setup_unavailable" {
				job["worktreePath"] = filepath.Join(root, "missing-worktree")
			}
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), job)
			if !tc.stale {
				writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
					"name": "codex", "agent": "codex", "args": map[string]any{"cwd": root, "prompt": "hello"},
				})
			}
			_ = os.Setenv("PURSER_HOST_FAKE_WORKER", "1")
			_ = os.Setenv("PURSER_HOST_FAKE_MODE", tc.mode)
			defer os.Unsetenv("PURSER_HOST_FAKE_WORKER")
			defer os.Unsetenv("PURSER_HOST_FAKE_MODE")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			options := HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
				if tc.name == "worker_launch_failure" {
					return []string{filepath.Join(root, "absent-worker")}, nil
				}
				return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
			}}
			finished := make(chan error, 1)
			if tc.cancel {
				go func() {
					_, hostErr := RunHostGeneration(dir, hostTestID, 1, "owner", options)
					finished <- hostErr
				}()
				waitForHostState(t, dir, "RUNNING")
				record := readJob(dir, hostTestID)
				var port int
				record.Get("signalPort", &port)
				ok, signalErr := SignalCancel("127.0.0.1:"+strconv.Itoa(port), hostTestID, "signal", time.Second)
				if signalErr != nil || !ok {
					var hostErr error
					select {
					case hostErr = <-finished:
					default:
					}
					t.Fatalf("cancel signal failed: ack=%t err=%v hostErr=%v record=%#v", ok, signalErr, hostErr, recordMap(readJob(dir, hostTestID)))
				}
				if err := <-finished; err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := RunHostGeneration(dir, hostTestID, 1, "owner", options); err != nil {
					t.Fatal(err)
				}
			}
			got := readJob(dir, hostTestID)
			if tc.name == "worker_launch_failure" {
				if stopped, err := recordedTreeStopped(dir, got); !stopped {
					t.Fatalf("launch failure tree unconfirmed: %v", err)
				}
			}
			var state, code string
			got.Get("state", &state)
			got.Get("errorCode", &code)
			if state != tc.state || code != tc.errorCode {
				t.Fatalf("terminal record state/code = %s/%s, want %s/%s", state, code, tc.state, tc.errorCode)
			}
			if tc.stale {
				if _, err = os.Stat(filepath.Join(dir, hostTestID+".launch")); !os.IsNotExist(err) {
					t.Fatalf("stale generation attempted to use launch record: err=%v", err)
				}
			}
			if tc.name == "success" {
				var thread, output string
				got.Get("threadId", &thread)
				got.Get("outputFile", &output)
				data, readErr := os.ReadFile(filepath.Join(dir, output))
				if readErr != nil || thread != "fake-thread" || string(data) != "fake answer" {
					t.Fatalf("success fields mismatch: thread=%q output=%q err=%v", thread, data, readErr)
				}
				var evidence []map[string]any
				if !got.Get("testEvidence", &evidence) || len(evidence) != 1 || evidence[0]["command"] != "go test ./..." || evidence[0]["exit_code"] != float64(0) {
					t.Fatalf("test evidence missing from completed record: %#v", evidence)
				}
				threadRecord := LoadRecord(filepath.Join(root, "codex_exec_mcp", "threads"), thread)
				if threadRecord["cwd"] != root || threadRecord["thread_id"] != thread {
					t.Fatalf("thread resume record missing: %#v", threadRecord)
				}
			}
		})
	}
}

func TestRunHostGenerationDefaultsMissingCWDToRepositoryRoot(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitStart(t, repo, "init", "-q")
	if err = os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex", "agent": "codex", "args": map[string]any{"prompt": "hello"},
	})
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options := HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
		argv, buildErr := BuildCodexCommand(LaunchArgs{BinaryOverride: "codex", Name: args.Name, Cwd: args.Cwd})
		if buildErr != nil {
			return nil, buildErr
		}
		for i, value := range argv {
			if value == "-C" && (i+1 == len(argv) || filepath.Clean(strings.ReplaceAll(argv[i+1], "/", string(filepath.Separator))) != filepath.Clean(repo)) {
				t.Errorf("Codex cwd argv=%#v, want repository root %q", argv, repo)
			}
		}
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}}
	if _, err = RunHostGeneration(dir, hostTestID, 1, "owner", options); err != nil {
		t.Fatal(err)
	}
}

func TestRunHostReplyUsesRecordedRepositoryAfterMainAdvances(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitStart(t, repo, "init", "-q")
	gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "base")
	worktree := filepath.Join(t.TempDir(), "reply-worktree")
	gitStart(t, repo, "worktree", "add", "-qb", "reply", worktree, "HEAD")
	gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "main advanced")
	serverCWD := t.TempDir()
	if err := os.Chdir(serverCWD); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	priorID := strings.Repeat("a", 64)
	writeHostTestJSON(t, filepath.Join(dir, priorID+".json"), map[string]any{
		"jobId": priorID, "state": "SUCCEEDED", "threadId": "reply-thread", "repoRoot": repo,
		"worktreePath": worktree, "finishedAt": float64(time.Now().UnixNano()) / 1e9,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
		"threadId": "reply-thread", "repoRoot": repo, "worktreePath": worktree,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex-reply", "agent": "codex", "args": map[string]any{"prompt": "continue", "threadId": "reply-thread"},
	})
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options := HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
		if filepath.Clean(args.Cwd) != filepath.Clean(worktree) {
			t.Errorf("reply cwd = %q, want existing worktree %q", args.Cwd, worktree)
		}
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}}
	if _, err := RunHostGeneration(dir, hostTestID, 1, "owner", options); err != nil {
		t.Fatal(err)
	}
	got := readJob(dir, hostTestID)
	var state string
	got.Get("state", &state)
	if state != "SUCCEEDED" {
		t.Fatalf("reply state = %s, want SUCCEEDED", state)
	}
}

func TestStartReplyUsesRecordedThreadWorktreeRepository(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitStart(t, repo, "init", "-q")
	gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "base")
	worktree := filepath.Join(t.TempDir(), "reply-worktree")
	gitStart(t, repo, "worktree", "add", "-qb", "reply", worktree, "HEAD")
	serverCWD := t.TempDir()
	if err = os.Chdir(serverCWD); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	state := t.TempDir()
	dir := filepath.Join(state, "codex_exec_mcp", "jobs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	priorID := strings.Repeat("a", 64)
	writeHostTestJSON(t, filepath.Join(dir, priorID+".json"), map[string]any{
		"jobId": priorID, "state": "SUCCEEDED", "threadId": "reply-thread", "repoRoot": repo,
		"worktreePath": worktree, "finishedAt": float64(time.Now().UnixNano()) / 1e9,
	})
	if err = SaveRecord(filepath.Join(state, "codex_exec_mcp", "threads"), map[string]any{
		"thread_id": "reply-thread", "cwd": worktree, "config_args": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var launched bool
	opt := StartOptions{StateRoot: state, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true},
		Launch: func(id string, generation int64, token string) error {
			launched = true
			_, hostErr := RunHostGeneration(dir, id, generation, token, HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
				if filepath.Clean(args.Cwd) != filepath.Clean(worktree) {
					t.Errorf("reply cwd = %q, want recorded worktree %q", args.Cwd, worktree)
				}
				return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
			}})
			return hostErr
		}}
	_, _, err = Start(state, map[string]any{"call": "00000000-0000-4000-8000-000000000001", "prompt": "continue", "mode": "reply", "threadId": "reply-thread"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !launched {
		t.Fatal("reply host was not launched")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var replied *Record
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") && entry.Name() != priorID+".json" {
			replied, err = Read(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if replied == nil {
		t.Fatal("reply job record was not written")
	}
	var stateValue string
	if !replied.Get("state", &stateValue) || stateValue != "SUCCEEDED" {
		t.Fatalf("reply state = %q, want SUCCEEDED", stateValue)
	}
}

func TestStartReplyUsesRecordedRepositoryForNonWorktreeThread(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	gitStart(t, repo, "init", "-q")
	gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "base")
	if err = os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	state := t.TempDir()
	dir := filepath.Join(state, "codex_exec_mcp", "jobs")
	var launched []string
	opt := StartOptions{StateRoot: state, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true},
		Launch: func(id string, generation int64, token string) error {
			launch, readErr := os.ReadFile(filepath.Join(dir, id+".launch"))
			if readErr != nil {
				return readErr
			}
			var value struct {
				Args map[string]any `json:"args"`
			}
			if readErr = json.Unmarshal(launch, &value); readErr != nil {
				return readErr
			}
			cwd, _ := value.Args["cwd"].(string)
			launched = append(launched, cwd)
			_, hostErr := RunHostGeneration(dir, id, generation, token, HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
				job := recordMap(readJob(dir, id))
				if args.Name == "codex-reply" {
					if sameWorktree(args.Cwd, repo) || !sameWorktree(args.Cwd, nonemptyString(job["worktreePath"])) || nonemptyString(job["snapshot"]) == "" {
						t.Errorf("reply did not get an isolated snapshot: cwd=%q job=%#v", args.Cwd, job)
					}
				} else if !sameWorktree(args.Cwd, repo) || job["worktreePath"] != nil {
					t.Errorf("initial non-worktree launch changed: cwd=%q job=%#v", args.Cwd, job)
				}
				executable, executableErr := os.Executable()
				if executableErr != nil {
					return nil, executableErr
				}
				return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
			}})
			return hostErr
		},
	}
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "")
	_, first, err := Start(state, map[string]any{"call": "00000000-0000-4000-8000-000000000011", "prompt": "start", "cwd": repo, "worktree": false}, opt)
	if err != nil {
		t.Fatal(err)
	}
	firstMap := recordMap(readJob(dir, nonemptyString(recordMap(first)["jobId"])))
	if filepath.Clean(nonemptyString(firstMap["repoRoot"])) != filepath.Clean(repo) {
		t.Fatalf("first job repoRoot = %#v, want %q", firstMap["repoRoot"], repo)
	}
	thread := LoadRecord(filepath.Join(state, "codex_exec_mcp", "threads"), "fake-thread")
	if thread == nil || filepath.Clean(nonemptyString(thread["repoRoot"])) != filepath.Clean(repo) {
		t.Fatalf("thread did not record repository root: %#v", thread)
	}
	_, reply, err := Start(state, map[string]any{"call": "00000000-0000-4000-8000-000000000012", "prompt": "continue", "mode": "reply", "threadId": "fake-thread"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	reply = readJob(dir, nonemptyString(recordMap(reply)["jobId"]))
	if nonemptyString(recordMap(reply)["worktreePath"]) == "" || recordMap(reply)["state"] != "SUCCEEDED" || len(launched) != 2 || !sameWorktree(launched[1], repo) {
		t.Fatalf("reply did not create a worktree from the recorded root; launches=%#v job=%s", launched, mustRecordJSON(t, reply))
	}

	oldState := t.TempDir()
	if err = SaveRecord(filepath.Join(oldState, "codex_exec_mcp", "threads"), map[string]any{
		"thread_id": "old-thread", "cwd": repo, "config_args": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	result, _, err := Start(oldState, map[string]any{"call": "00000000-0000-4000-8000-000000000013", "prompt": "continue", "mode": "reply", "threadId": "old-thread", "cwd": repo, "worktree": false}, StartOptions{StateRoot: oldState, Launch: func(string, int64, string) error {
		t.Fatal("reply without a recorded repository launched")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	failure, _ := result["structuredContent"].(map[string]any)
	if failure["errorCode"] != "WORKTREE_SNAPSHOT_FAILED" || failure["message"] != "Repository root could not be resolved for Codex launch" {
		t.Fatalf("rootless old thread error = %#v", result)
	}
}

func TestReplyAfterCompletedRetirementStaysIsolated(t *testing.T) {
	for _, creationFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("creationFails=%t", creationFails), func(t *testing.T) {
			state := jobsScratchDir(t, "retired-reply-")
			repo := cleanupRepo(t, state)
			worktree := cleanupWT(t, state, repo, "retiring")
			priorID := strings.Repeat("a", 64)
			cleanupJob(t, state, priorID, "SUCCEEDED", map[string]any{
				"threadId": "reply-thread", "repoRoot": repo, "worktreePath": worktree, "applyState": "applied",
			})
			threads := filepath.Join(state, "codex_exec_mcp", "threads")
			if err := SaveRecord(threads, map[string]any{
				"thread_id": "reply-thread", "cwd": worktree, "repoRoot": repo, "worktreePath": worktree, "config_args": []any{},
			}); err != nil {
				t.Fatal(err)
			}
			if ok, err := retention.AttachWorktree(state, worktree, priorID, "reply-thread"); err != nil || !ok {
				t.Fatalf("attach = %v, %v", ok, err)
			}
			if _, err := retention.MarkWorktreeRetiring(state, worktree); err != nil {
				t.Fatal(err)
			}
			if _, err := retention.UpdateWorktreeOwnership(state, worktree, func(record *retention.WorktreeOwnership) { record.Claimed = true }); err != nil {
				t.Fatal(err)
			}
			if err := RetireWorktree(state, priorID); err != nil {
				t.Fatal(err)
			}
			if pathExists(worktree) || nonemptyString(LoadRecord(threads, "reply-thread")["worktreePath"]) != "" {
				t.Fatal("retirement did not clear the worktree binding")
			}
			if err := os.WriteFile(filepath.Join(repo, "reply.txt"), []byte("host advanced"), 0600); err != nil {
				t.Fatal(err)
			}
			cleanupGit(t, repo, "add", "reply.txt")
			cleanupGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "host advanced")
			head := cleanupGit(t, repo, "rev-parse", "HEAD")
			dir := filepath.Join(state, "codex_exec_mcp", "jobs")
			t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
			t.Setenv("PURSER_HOST_FAKE_MODE", "edit")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			workerLaunched := false
			opt := StartOptions{StateRoot: state, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true},
				Launch: func(id string, generation int64, token string) error {
					if creationFails {
						if err := os.WriteFile(filepath.Join(state, "wt", id[:12]), []byte("blocked"), 0600); err != nil {
							return err
						}
					}
					_, hostErr := RunHostGeneration(dir, id, generation, token, HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
						workerLaunched = true
						job := recordMap(readJob(dir, id))
						if sameWorktree(args.Cwd, repo) || sameWorktree(args.Cwd, worktree) || !sameWorktree(args.Cwd, nonemptyString(job["worktreePath"])) || nonemptyString(job["snapshot"]) == "" || job["head"] != head {
							t.Fatalf("reply escaped fresh isolation: cwd=%q job=%#v", args.Cwd, job)
						}
						return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
					}})
					return hostErr
				}}
			_, reply, err := Start(state, map[string]any{
				"call": "00000000-0000-4000-8000-000000000014", "prompt": "continue", "mode": "reply", "threadId": "reply-thread", "cwd": repo, "worktree": false,
			}, opt)
			if err != nil || reply == nil {
				t.Fatalf("reply start: record=%v err=%v", reply, err)
			}
			job := recordMap(readJob(dir, nonemptyString(recordMap(reply)["jobId"])))
			if creationFails {
				if workerLaunched || job["state"] != "FAILED" || job["errorCode"] != "WORKTREE_SNAPSHOT_FAILED" || job["message"] != "Worktree snapshot could not be created" {
					t.Fatalf("creation failure did not refuse worker launch: launched=%v job=%#v", workerLaunched, job)
				}
			} else {
				if !workerLaunched || job["state"] != "SUCCEEDED" {
					t.Fatalf("isolated reply did not succeed: %#v", job)
				}
				if edited, err := os.ReadFile(filepath.Join(nonemptyString(job["worktreePath"]), "reply.txt")); err != nil || string(edited) != "reply edit" {
					t.Fatalf("worker edit = %q, %v", edited, err)
				}
			}
			if host, err := os.ReadFile(filepath.Join(repo, "reply.txt")); err != nil || string(host) != "host advanced" {
				t.Fatalf("reply changed host checkout: %q, %v", host, err)
			}
		})
	}
}

func TestStartReplyUsesFreshWorktreeWhenThreadWorktreeIsMissingWithoutOwnership(t *testing.T) {
	repo := t.TempDir()
	gitStart(t, repo, "init", "-q")
	gitStart(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "base")
	missingWorktree := filepath.Join(t.TempDir(), "missing-worktree")
	state := t.TempDir()
	dir := filepath.Join(state, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	priorID := strings.Repeat("a", 64)
	writeHostTestJSON(t, filepath.Join(dir, priorID+".json"), map[string]any{
		"jobId": priorID, "state": "SUCCEEDED", "threadId": "reply-thread", "repoRoot": repo,
		"worktreePath": missingWorktree, "finishedAt": float64(time.Now().UnixNano()) / 1e9,
	})
	if err := SaveRecord(filepath.Join(state, "codex_exec_mcp", "threads"), map[string]any{
		"thread_id": "reply-thread", "cwd": missingWorktree, "config_args": []any{},
		"worktreePath": missingWorktree, "repoRoot": repo,
	}); err != nil {
		t.Fatal(err)
	}
	var launched bool
	opt := StartOptions{StateRoot: state, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true},
		Launch: func(id string, _ int64, _ string) error {
			launched = true
			job := readJob(dir, id)
			if _, ok := recordMap(job)["worktreePath"]; ok {
				t.Fatalf("reply inherited missing worktree: %s", mustRecordJSON(t, job))
			}
			return nil
		}, Await: func(id string, _ *Record) map[string]any {
			return jobResult(map[string]any{"jobId": id, "state": "QUEUED"})
		}}
	_, _, err := Start(state, map[string]any{"call": "00000000-0000-0000-0000-000000000001", "prompt": "continue", "mode": "reply", "threadId": "reply-thread"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !launched {
		t.Fatal("reply host was not launched")
	}
}

func TestRunHostGenerationWritesValidFailureRecord(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex-reply", "agent": "codex", "args": map[string]any{"prompt": "continue", "threadId": "reply-thread"},
	})
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if _, err = RunHostGeneration(dir, hostTestID, 1, "owner"); err != nil {
		t.Fatal(err)
	}
	failure, err := Read(filepath.Join(dir, hostTestID+".json"))
	if err != nil {
		t.Fatalf("failure record could not be decoded: %v", err)
	}
	var stateValue, code, message string
	if !failure.Get("state", &stateValue) || stateValue != "FAILED" ||
		!failure.Get("errorCode", &code) || code != "WORKTREE_SNAPSHOT_FAILED" ||
		!failure.Get("message", &message) || message != "Repository root could not be resolved for Codex launch" {
		t.Fatalf("invalid launch failure record: %s", mustRecordJSON(t, failure))
	}
}

func TestHostThreadOwnershipUsesExplicitStateRoot(t *testing.T) {
	stateRoot := t.TempDir()
	jobsRoot := t.TempDir()
	jobDir := filepath.Join(jobsRoot, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(stateRoot, "wt", "owned")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	writeHostTestJSON(t, filepath.Join(jobDir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "RUNNING", "generation": 1, "ownershipToken": "owner",
	})
	if err := saveHostThread(jobDir, hostTestID, 1, "owner", "thread", map[string]any{
		"worktreePath": worktree,
	}, true, stateRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.ReadWorktreeOwnership(stateRoot, worktree); err != nil {
		t.Fatalf("ownership was not stored under Purser state root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobsRoot, "worktree_ownership")); !os.IsNotExist(err) {
		t.Fatalf("ownership leaked beside relative/custom job directory: %v", err)
	}
}

func TestQueuedAdmissionBaselineFailureIsTerminalAndDetailed(t *testing.T) {
	root := admissionTempDir(t)
	setupAdmission(t, root, "queued")
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	jobID := admissionJobID()
	job := readJob(dir, jobID)
	for key, value := range map[string]any{
		"generation": 1, "ownershipToken": "owner", "signalToken": "signal",
		"deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9, "leaseQueued": true,
	} {
		if err := job.Set(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
		t.Fatal(err)
	}
	launch := map[string]any{"name": "codex", "agent": "codex", "args": map[string]any{"cwd": filepath.Join(root, "repo"), "prompt": "unused"}}
	writeHostTestJSON(t, filepath.Join(dir, jobID+".launch"), launch)
	pendingPath := filepath.Join(root, "pending", admissionOwner()+".json")
	pending := admissionJSON(t, pendingPath).(map[string]any)
	pending["repo_root"] = filepath.Join(root, "missing-repo")
	writeHostTestJSON(t, pendingPath, pending)
	status, err := RunHostGeneration(dir, jobID, 1, "owner", HostOptions{WorkerCommand: func(LaunchArgs) ([]string, error) {
		return []string{"unused"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	final := recordMap(readJob(dir, jobID))
	if status["errorCode"] != "ADMISSION_BASELINE_UNAVAILABLE" || final["state"] != "FAILED" ||
		final["errorCode"] != "ADMISSION_BASELINE_UNAVAILABLE" || !strings.Contains(final["message"].(string), "GIT_UNAVAILABLE:") {
		t.Fatalf("status=%#v record=%#v", status, final)
	}
}

func mustRecordJSON(t *testing.T, record *Record) string {
	t.Helper()
	data, err := record.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestConfigureCodexGoCacheWritableRootsIncludeTelemetryWhenPresent(t *testing.T) {
	for _, exists := range []bool{true, false} {
		name := "missing"
		if exists {
			name = "present"
		}
		t.Run(name, func(t *testing.T) {
			configRoot := t.TempDir()
			t.Setenv("APPDATA", configRoot)
			t.Setenv("XDG_CONFIG_HOME", configRoot)
			t.Setenv("GOCACHE", filepath.Join(configRoot, "custom-cache"))
			t.Setenv("GOTMPDIR", filepath.Join(configRoot, "custom-tmp"))
			telemetryDir := filepath.Join(configRoot, "go", "telemetry")
			if exists {
				if err := os.MkdirAll(telemetryDir, 0700); err != nil {
					t.Fatal(err)
				}
			}

			var childEnv []string
			args := LaunchArgs{}
			jobTempDir := filepath.Join(configRoot, "cache", "tmp", hostTestID)
			if err := configureCodexGoCache(&args, &childEnv, configRoot, jobTempDir, nil); err != nil {
				t.Fatal(err)
			}
			config := args.Config.(map[string]any)
			var roots []any
			if workspace, ok := config["sandbox_workspace_write"].(map[string]any); ok {
				roots, _ = workspace["writable_roots"].([]any)
			}
			if exists {
				if len(roots) != 2 || roots[0] != telemetryDir || roots[1] != jobTempDir {
					t.Errorf("writable roots = %#v, want [%q %q]", roots, telemetryDir, jobTempDir)
				}
			} else if len(roots) != 1 || roots[0] != jobTempDir {
				t.Errorf("writable roots = %#v, want [%q]", roots, jobTempDir)
			}
		})
	}
}

func TestHostAddsSharedGoCacheForWorkspaceWrite(t *testing.T) {
	t.Setenv("GOCACHE", "")
	t.Setenv("GOTMPDIR", "")
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	deadline := float64(time.Now().Add(time.Minute).UnixNano()) / 1e9
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": deadline, "worktreePath": worktree, "repoRoot": worktree,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex", "agent": "codex", "args": map[string]any{
			"cwd": worktree, "prompt": "hello", "config": map[string]any{
				"sandbox_workspace_write":                map[string]any{"writable_roots": []any{"outside"}},
				"sandbox_workspace_write.network_access": true,
				"sandbox_workspace_write.extra":          "unsafe",
				"unrelated":                              "preserved",
			},
		},
	})
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "environment")
	environmentCapture := filepath.Join(root, "child-environment.json")
	t.Setenv("PURSER_HOST_FAKE_ENV_CAPTURE", environmentCapture)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(root, "cache", "go-build")
	tmpDir := filepath.Join(root, "cache", "tmp", hostTestID)
	options := HostOptions{StateRoot: root, WorkerCommand: func(args LaunchArgs) ([]string, error) {
		config, ok := args.Config.(map[string]any)
		if !ok {
			t.Fatalf("config has type %T", args.Config)
		}
		if _, exists := config["sandbox_workspace_write.network_access"]; exists {
			t.Error("dotted sandbox_workspace_write key survived host rewrite")
		}
		if _, exists := config["sandbox_workspace_write.extra"]; exists {
			t.Error("dotted sandbox_workspace_write key survived host rewrite")
		}
		if config["unrelated"] != "preserved" {
			t.Errorf("unrelated config was lost: %#v", config)
		}
		workspace, _ := config["sandbox_workspace_write"].(map[string]any)
		if roots, _ := workspace["writable_roots"].([]any); len(roots) != 4 || roots[0] != worktree || roots[1] != "outside" || roots[2] != cacheDir || roots[3] != tmpDir {
			t.Errorf("writable roots = %#v, want [%q outside %q %q]", roots, worktree, cacheDir, tmpDir)
		}
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}}
	if _, err := RunHostGeneration(dir, hostTestID, 1, "owner", options); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{cacheDir} {
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			t.Errorf("cache directory %q was not created: %v", directory, err)
		}
	}
	var childEnvironment map[string]string
	childOutput, err := os.ReadFile(environmentCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(childOutput, &childEnvironment); err != nil {
		t.Fatal(err)
	}
	if childEnvironment["GOCACHE"] != cacheDir || childEnvironment["GOTMPDIR"] != tmpDir || childEnvironment["TEMP"] != tmpDir || childEnvironment["TMP"] != tmpDir || childEnvironment["TMPDIR"] != tmpDir {
		t.Errorf("child Go environment = %#v, want per-job temp %q and GOCACHE=%q", childEnvironment, tmpDir, cacheDir)
	}
	if _, err := os.Stat(tmpDir); !os.IsNotExist(err) {
		t.Errorf("terminal job temp directory remains: %v", err)
	}
}

func TestHostAddsSharedGoCacheWithoutWorktree(t *testing.T) {
	t.Setenv("GOCACHE", "")
	t.Setenv("GOTMPDIR", "")
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	deadline := float64(time.Now().Add(time.Minute).UnixNano()) / 1e9
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": deadline, "repoRoot": root,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex", "agent": "codex", "args": map[string]any{
			"cwd": root, "prompt": "hello", "config": map[string]any{
				"sandbox_workspace_write": map[string]any{"writable_roots": []any{"existing"}},
			},
		},
	})
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "environment")
	environmentCapture := filepath.Join(root, "child-environment.json")
	t.Setenv("PURSER_HOST_FAKE_ENV_CAPTURE", environmentCapture)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(root, "cache", "go-build")
	tmpDir := filepath.Join(root, "cache", "tmp", hostTestID)
	options := HostOptions{StateRoot: root, WorkerCommand: func(args LaunchArgs) ([]string, error) {
		config := map[string]any{}
		switch ordered := args.Config.(type) {
		case OrderedObject:
			for _, entry := range ordered {
				config[entry.Key] = entry.Value
			}
		case map[string]any:
			config = ordered
		default:
			t.Fatalf("config has type %T", args.Config)
		}
		workspace, _ := config["sandbox_workspace_write"].(map[string]any)
		if roots, _ := workspace["writable_roots"].([]any); len(roots) != 3 || roots[0] != "existing" || roots[1] != cacheDir || roots[2] != tmpDir {
			t.Errorf("writable roots = %#v, want [existing %q %q]", roots, cacheDir, tmpDir)
		}
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}}
	if _, err := RunHostGeneration(dir, hostTestID, 1, "owner", options); err != nil {
		t.Fatal(err)
	}
	var childEnvironment map[string]string
	childOutput, err := os.ReadFile(environmentCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(childOutput, &childEnvironment); err != nil {
		t.Fatal(err)
	}
	if childEnvironment["GOCACHE"] != cacheDir || childEnvironment["GOTMPDIR"] != tmpDir || childEnvironment["TEMP"] != tmpDir || childEnvironment["TMP"] != tmpDir || childEnvironment["TMPDIR"] != tmpDir {
		t.Errorf("child Go environment = %#v, want per-job temp %q and GOCACHE=%q", childEnvironment, tmpDir, cacheDir)
	}
}

func TestHostPreservesExplicitGoCacheEnvironment(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	worktree := filepath.Join(root, "worktree")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(root, "user-cache")
	tmpDir := filepath.Join(root, "user-tmp")
	t.Setenv("GOCACHE", cacheDir)
	t.Setenv("GOTMPDIR", tmpDir)
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "environment")
	environmentCapture := filepath.Join(root, "child-environment.json")
	t.Setenv("PURSER_HOST_FAKE_ENV_CAPTURE", environmentCapture)
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9, "worktreePath": worktree, "repoRoot": worktree,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex", "agent": "codex", "args": map[string]any{"cwd": worktree, "prompt": "hello"},
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunHostGeneration(dir, hostTestID, 1, "owner", HostOptions{StateRoot: root, WorkerCommand: func(args LaunchArgs) ([]string, error) {
		config := args.Config.(map[string]any)
		workspace := config["sandbox_workspace_write"].(map[string]any)
		if roots, _ := workspace["writable_roots"].([]any); len(roots) != 2 || roots[0] != worktree || roots[1] != filepath.Join(root, "cache", "tmp", hostTestID) {
			t.Errorf("writable roots = %#v, want %q and per-job temp", roots, worktree)
		}
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var childEnvironment map[string]string
	childOutput, err := os.ReadFile(environmentCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(childOutput, &childEnvironment); err != nil {
		t.Fatal(err)
	}
	jobTempDir := filepath.Join(root, "cache", "tmp", hostTestID)
	if childEnvironment["GOCACHE"] != cacheDir || childEnvironment["GOTMPDIR"] != tmpDir || childEnvironment["TEMP"] != jobTempDir || childEnvironment["TMP"] != jobTempDir || childEnvironment["TMPDIR"] != jobTempDir {
		t.Errorf("child Go environment = %#v, want explicit Go cache values and per-job temp %q", childEnvironment, jobTempDir)
	}
	if _, err := os.Stat(filepath.Join(root, "cache", "go-build")); !os.IsNotExist(err) {
		t.Errorf("Purser GOCACHE root should not be created for explicit GOCACHE, stat error = %v", err)
	}
}

func TestHostStartFailurePreservesCancellationAndDeadline(t *testing.T) {
	tests := []struct {
		name, state, code string
		cancelled         bool
		contextErr        error
	}{
		{name: "cancel", state: "CANCELLED", code: "CANCELLED", cancelled: true, contextErr: context.Canceled},
		{name: "deadline", state: "TIMED_OUT", code: "DEADLINE_EXCEEDED", contextErr: context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
				"jobId": hostTestID, "state": "RUNNING", "generation": 1, "ownershipToken": "owner",
			})
			if _, err := finishHostStartError(dir, hostTestID, 1, "owner", tc.cancelled, tc.contextErr, errors.New("worker start failed")); err != nil {
				t.Fatal(err)
			}
			got := readJob(dir, hostTestID)
			var state, code string
			got.Get("state", &state)
			got.Get("errorCode", &code)
			if state != tc.state || code != tc.code {
				t.Fatalf("start failure terminal state/code=%s/%s, want %s/%s", state, code, tc.state, tc.code)
			}
		})
	}
}

func waitForHostState(t *testing.T, dir, expected string) {
	t.Helper()
	limit := time.Now().Add(3 * time.Second)
	for time.Now().Before(limit) {
		job := readJob(dir, hostTestID)
		if job == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		var state string
		job.Get("state", &state)
		if state == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("host did not enter RUNNING")
}

func writeHostTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
