package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const hostTestID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHostFakeWorker(t *testing.T) {
	if os.Getenv("PURSER_HOST_FAKE_WORKER") != "1" {
		return
	}
	output := os.Args[len(os.Args)-1]
	mode := os.Getenv("PURSER_HOST_FAKE_MODE")
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
	_, _ = os.Stdout.WriteString("{\"type\": \"turn.completed\"}\r\n")
	_ = os.WriteFile(output, []byte("fake answer"), 0600)
	if mode == "nonzero" {
		os.Exit(7)
	}
	os.Exit(0)
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
		{name: "deadline", mode: "sleep", state: "TIMED_OUT", errorCode: "DEADLINE_EXCEEDED", deadline: 120 * time.Millisecond},
		{name: "deadline_before_worker_start", state: "TIMED_OUT", errorCode: "DEADLINE_EXCEEDED", deadline: -time.Second},
		{name: "signal_cancel", mode: "sleep", state: "CANCELLED", errorCode: "CANCELLED", deadline: 5 * time.Second, cancel: true},
		{name: "stale_generation", state: "QUEUED", stale: true},
		{name: "worktree_setup_unavailable", state: "FAILED", errorCode: "WORKTREE_MISSING"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
					t.Fatalf("cancel signal failed: ack=%t err=%v", ok, signalErr)
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
				threadRecord := LoadRecord(filepath.Join(root, "codex_exec_mcp", "threads"), thread)
				if threadRecord["cwd"] != root || threadRecord["thread_id"] != thread {
					t.Fatalf("thread resume record missing: %#v", threadRecord)
				}
			}
		})
	}
}

func TestHostReplacesDottedSandboxWorkspaceWriteKeys(t *testing.T) {
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
	t.Setenv("PURSER_HOST_FAKE_MODE", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options := HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
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
		if roots, _ := workspace["writable_roots"].([]any); len(roots) != 1 || roots[0] != worktree {
			t.Errorf("writable root = %#v, want %q", roots, worktree)
		}
		return []string{executable, "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}}
	if _, err := RunHostGeneration(dir, hostTestID, 1, "owner", options); err != nil {
		t.Fatal(err)
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
