package jobs

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each real child records its input, so this counts launches rather than calls
// to the command builder (which must run only once).
func TestCapacityRetryWorker(t *testing.T) {
	if os.Getenv("PURSER_CAPACITY_WORKER") != "1" {
		return
	}
	args := os.Args[len(os.Args)-3:]
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	previous, _ := os.ReadFile(args[1])
	cwd, _ := os.Getwd()
	entry, _ := json.Marshal([]string{string(prompt), args[2], cwd})
	if err := os.WriteFile(args[1], append(append(previous, entry...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv("PURSER_CAPACITY_MODE")
	if mode != "recover" || len(previous) == 0 {
		message := "HTTP 429 rate limited; Retry-After: 0"
		if mode == "ambiguous" {
			message = "connection lost"
		}
		_, _ = os.Stderr.WriteString(message)
		if mode == "stdout" {
			_, _ = os.Stdout.WriteString("partial answer")
		}
		if mode == "output" {
			_ = os.WriteFile(args[0], []byte("partial answer"), 0600)
		}
		os.Exit(1)
	}
	_, _ = os.Stdout.WriteString("{\"type\":\"thread.started\",\"thread_id\":\"original-thread\"}\n{\"type\":\"turn.completed\"}\n")
	_ = os.WriteFile(args[0], prompt, 0600)
	os.Exit(0)
}

func TestRunHostCapacityRetries(t *testing.T) {
	for _, tc := range []struct {
		name, mode, state, code string
		prior                   any
		starts, attempts        int
		cancel                  bool
	}{
		{name: "same_intent", mode: "recover", state: "SUCCEEDED", starts: 2, attempts: 1},
		{name: "exhausted", state: "FAILED", code: "CAPACITY_RETRIES_EXHAUSTED", starts: 3, attempts: 2},
		{name: "persisted_budget", prior: 1, state: "FAILED", code: "CAPACITY_RETRIES_EXHAUSTED", starts: 2, attempts: 2},
		{name: "invalid_budget", prior: "invalid", state: "FAILED", code: "CODEX_FAILED", starts: 1},
		{name: "stdout", mode: "stdout", state: "FAILED", code: "CODEX_FAILED", starts: 1},
		{name: "output", mode: "output", state: "FAILED", code: "CODEX_FAILED", starts: 1},
		{name: "ambiguous", mode: "ambiguous", state: "FAILED", code: "CODEX_FAILED", starts: 1},
		{name: "cancel_backoff", state: "CANCELLED", code: "CANCELLED", starts: 1, attempts: 1, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := scratch(t)
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			fields := map[string]any{
				"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
				"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
				"threadId": "original-thread", "worktreePath": root, "repoRoot": root,
			}
			if tc.prior != nil {
				fields["capacityRetryAttempts"] = tc.prior
			}
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), fields)
			prompt := "exact UTF-8: café 雪\n"
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
				"name": "codex-reply", "agent": "codex", "args": map[string]any{
					"cwd": root, "prompt": prompt, "model": "fixed-model", "threadId": "original-thread",
					"config": map[string]any{"model_reasoning_effort": "high"},
				},
			})
			t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
			t.Setenv("PURSER_CAPACITY_WORKER", "1")
			t.Setenv("PURSER_CAPACITY_MODE", tc.mode)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(root, "attempts")
			builds := 0
			opt := HostOptions{HostContained: true, WorkerCommand: func(args LaunchArgs) ([]string, error) {
				builds++
				config, err := FlattenConfig(args.Config)
				if err != nil || args.Model != "fixed-model" || args.ThreadID != "original-thread" || args.Name != "codex-reply" || !strings.Contains(strings.Join(config, " "), "high") {
					t.Fatalf("changed launch intent: %#v, config=%v err=%v", args, config, err)
				}
				intent, _ := json.Marshal(args)
				return []string{executable, "-test.run=^TestCapacityRetryWorker$", "--", args.OutputPath, capture, string(intent)}, nil
			}}
			finished := make(chan error, 1)
			go func() {
				_, err := RunHostGeneration(dir, hostTestID, 1, "owner", opt)
				finished <- err
			}()
			// Observe the reserved retry, then try another host and recovery while
			// the owning host is alive and waiting in backoff.
			if tc.name == "same_intent" || tc.cancel {
				until := time.Now().Add(5 * time.Second)
				for {
					job := readJob(dir, hostTestID)
					attempts, _ := recoveryInteger(job, "capacityRetryAttempts")
					if attempts == 1 {
						break
					}
					if time.Now().After(until) {
						t.Fatal("retry was not reserved")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if tc.cancel {
					var port int
					readJob(dir, hostTestID).Get("signalPort", &port)
					ack, err := SignalCancel(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), hostTestID, "signal", time.Second)
					if err != nil || !ack {
						t.Fatalf("cancel: ack=%t err=%v", ack, err)
					}
				} else {
					if recovered := RecoverJob(root, hostTestID, float64(time.Now().UnixNano())/1e9, RecoverOptions{}); recovered != "ALIVE" {
						t.Fatalf("live host recovery = %s", recovered)
					}
					other, err := RunHostGeneration(dir, hostTestID, 1, "owner", opt)
					if err != nil || other["status"] != "stale" {
						t.Fatalf("duplicate host = %#v, %v", other, err)
					}
				}
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("host did not finish")
			}
			job := recordMap(readJob(dir, hostTestID))
			if job["state"] != tc.state || stringValue(job["errorCode"]) != tc.code || job["generation"] != float64(1) {
				t.Fatalf("outcome = %#v", job)
			}
			if tc.prior == "invalid" {
				if job["capacityRetryAttempts"] != tc.prior {
					t.Fatal("invalid budget was reset")
				}
			} else if int(numberOr(job["capacityRetryAttempts"], 0)) != tc.attempts {
				t.Fatalf("retry attempts = %v, want %d", job["capacityRetryAttempts"], tc.attempts)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != tc.starts || builds != 1 {
				t.Fatalf("starts=%d builds=%d, want %d/1", len(lines), builds, tc.starts)
			}
			for _, line := range lines {
				var entry []string
				if err := json.Unmarshal([]byte(line), &entry); err != nil || entry[0] != prompt || line != lines[0] {
					t.Fatalf("retry input changed: %q err=%v", line, err)
				}
			}
			if tc.state == "SUCCEEDED" && job["launchFailureEvidence"] != nil {
				t.Fatalf("successful retry retained failure marker: %v", job["launchFailureEvidence"])
			}
		})
	}
}

func TestRateLimitNoWorkProof(t *testing.T) {
	for _, tc := range []struct {
		name           string
		stdout, output bool
		events         []map[string]any
		proof          bool
	}{
		{name: "prework", proof: true},
		{name: "stdout", stdout: true},
		{name: "output", output: true},
		{name: "work_event", events: []map[string]any{{"type": "item.completed"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := LaunchFailureEvidence("codex", tc.events, "HTTP 429 rate limited", true, tc.stdout, tc.output, nil)
			if evidence["type"] != "RATE_LIMITED" || evidence["noWorkProof"] != tc.proof {
				t.Fatalf("evidence = %#v", evidence)
			}
		})
	}
}

func TestCapacityRetryDeadlineAndBackoff(t *testing.T) {
	root := scratch(t)
	path := filepath.Join(root, hostTestID+".json")
	writeHostTestJSON(t, path, map[string]any{
		"jobId": hostTestID, "state": "RUNNING", "generation": 1, "ownershipToken": "owner",
		"deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
	})
	after := 1000.0
	evidence := map[string]any{"type": "RATE_LIMITED", "noWorkProof": true, "retryAfterSeconds": &after}
	delay, retry, err := reserveCapacityRetry(path, hostTestID, 1, "owner", evidence)
	if err != nil || !retry || delay != 30*time.Second {
		t.Fatalf("backoff = %s/%t/%v", delay, retry, err)
	}
	job, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = job.Set("deadlineAt", float64(time.Now().Add(-time.Second).UnixNano())/1e9)
	if err := Write(path, job); err != nil {
		t.Fatal(err)
	}
	_, retry, err = reserveCapacityRetry(path, hostTestID, 1, "owner", evidence)
	job, _ = Read(path)
	attempts, _ := recoveryInteger(job, "capacityRetryAttempts")
	if err != nil || retry || attempts != 1 {
		t.Fatalf("expired retry = %t/%v attempts=%d", retry, err, attempts)
	}
}
