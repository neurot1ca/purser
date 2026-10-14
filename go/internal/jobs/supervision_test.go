package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/proc"
	"purser-hook/internal/retention"
)

// Reuse M3's same-binary supervisor and worker/leaf subprocess protocol through
// the existing host worker, rather than duplicating OS containment fixtures.
func TestHostTreeOwner(t *testing.T) {
	mode := os.Getenv("PURSER_HOST_TREE_OWNER")
	if mode == "" {
		return
	}
	root := os.Getenv("PURSER_HOST_TREE_ROOT")
	if mode == "launcher" {
		target := os.Getenv("PURSER_HOST_TREE_TARGET")
		if target == "" {
			target = "host"
		}
		env := replaceValidationEnv(os.Environ(), "PURSER_HOST_TREE_OWNER", target)
		pid, _, err := LaunchDetached([]string{os.Args[0], "-test.run=TestHostTreeOwner"}, env, root)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(pid)
		if err := os.WriteFile(filepath.Join(root, "owner"), data, 0600); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	if mode == "validation" {
		if err := writeApplyValidation(filepath.Join(root, hostTestID+".json"), applyValidation{JobID: hostTestID, Status: "running"}); err != nil {
			t.Fatal(err)
		}
		log, err := os.Create(filepath.Join(root, hostTestID+".log"))
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		if err := runValidationCommand(root, os.Environ(), []string{os.Args[0], "-test.run=TestHostFakeWorker", "--", filepath.Join(root, "output")}, time.Minute, log, nil); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	_, err := RunHostGeneration(dir, hostTestID, 1, "owner", HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
		return []string{os.Args[0], "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestHostDetachedLifetimeAndTreeStop(t *testing.T) {
	for _, mode := range []string{"cancel", "host-death", "validation-death", "validation-completion"} {
		t.Run(mode, func(t *testing.T) {
			root := jobsScratchDir(t, "host-lifetime-")
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			pidPath := filepath.Join(root, "leaf")
			env := replaceValidationEnv(os.Environ(), "PURSER_HOST_FAKE_WORKER", "1")
			env = replaceValidationEnv(env, "PURSER_HOST_FAKE_MODE", "tree-wait")
			env = replaceValidationEnv(env, "PURSER_HOST_TREE_PID", pidPath)
			env = replaceValidationEnv(env, "PURSER_HOST_TREE_ROOT", root)
			env = replaceValidationEnv(env, "PURSER_HOST_TREE_OWNER", "launcher")
			if runtime.GOOS == "windows" {
				env = replaceValidationEnv(env, "PURSER_CODEX_TEST_PARENT_PID", fmt.Sprint(os.Getpid()))
			}
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
				"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
				"signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9,
			})
			writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{"name": "codex", "args": map[string]any{"cwd": root}})
			if strings.HasPrefix(mode, "validation") {
				env = replaceValidationEnv(env, "PURSER_HOST_TREE_TARGET", "validation")
				if mode == "validation-completion" {
					env = replaceValidationEnv(env, "PURSER_HOST_FAKE_MODE", "tree")
				}
			}
			launcher := proc.Hide(exec.Command(os.Args[0], "-test.run=TestHostTreeOwner"))
			launcher.Env, launcher.Dir = env, root
			launcher.Stdout, launcher.Stderr = io.Discard, io.Discard
			if err := launcher.Start(); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "validation") {
				if err := launcher.Wait(); err != nil {
					t.Fatal(err)
				}
				owner := awaitHostLeaf(t, filepath.Join(root, "owner"))
				p, _ := os.FindProcess(owner)
				t.Cleanup(func() {
					if p != nil {
						_ = p.Kill()
						_ = p.Release()
					}
				})
				pid := awaitHostLeaf(t, pidPath)
				t.Cleanup(func() {
					p, _ := os.FindProcess(pid)
					if p != nil {
						_ = p.Kill()
						_ = p.Release()
					}
				})
				if mode == "validation-death" {
					_ = p.Kill()
				}
				awaitHostLeafStopped(t, pid)
				if mode == "validation-completion" {
					until := time.Now().Add(2 * time.Second)
					for time.Now().Before(until) {
						r, err := readApplyValidation(filepath.Join(root, hostTestID+".json"))
						if err == nil {
							if stopped, _ := treeStopped(root, hostTestID, r.TreeReceipt); stopped {
								return
							}
						}
						time.Sleep(10 * time.Millisecond)
					}
					t.Fatal("validation completion did not save tree confirmation")
				}
				return
			}
			if err := launcher.Wait(); err != nil {
				t.Fatal(err)
			}
			owner := awaitHostLeaf(t, filepath.Join(root, "owner"))
			p, _ := os.FindProcess(owner)
			t.Cleanup(func() {
				if p != nil {
					_ = p.Kill()
					_ = p.Release()
				}
			})
			pid := awaitHostLeaf(t, pidPath)
			t.Cleanup(func() {
				leaf, _ := os.FindProcess(pid)
				if leaf != nil {
					_ = leaf.Kill()
					_ = leaf.Release()
				}
			})
			if _, alive := hostLeafIdentity(pid); !alive {
				t.Fatal("session exit stopped durable work")
			}
			if mode == "cancel" {
				got := Cancel(root, map[string]any{"jobId": hostTestID})["structuredContent"].(map[string]any)
				if got["state"] != "CANCELLED" {
					t.Fatalf("cancel: %#v", got)
				}
			} else {
				if err := p.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			awaitHostLeafStopped(t, pid)
			if mode == "host-death" && runtime.GOOS != "windows" {
				job := readJob(dir, hostTestID)
				until := time.Now().Add(time.Second)
				for time.Now().Before(until) {
					if stopped, _ := recordedTreeStopped(dir, job); stopped {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("guard swept after host death without saving confirmation")
			}
		})
	}
}

func TestHostCompletionSweepsGrandchild(t *testing.T) {
	root := jobsScratchDir(t, "host-tree-")
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(root, "leaf")
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "tree")
	t.Setenv("PURSER_HOST_TREE_PID", pidPath)
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner",
		"signalToken": "signal", "deadlineAt": float64(time.Now().Add(5*time.Second).UnixNano()) / 1e9,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{
		"name": "codex", "args": map[string]any{"cwd": root},
	})
	done := make(chan error, 1)
	go func() {
		_, err := RunHostGeneration(dir, hostTestID, 1, "owner", HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
			return []string{os.Args[0], "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
		}})
		done <- err
	}()
	pid := awaitHostLeaf(t, pidPath)
	t.Cleanup(func() {
		p, _ := os.FindProcess(pid)
		if p != nil {
			_ = p.Kill()
			_ = p.Release()
		}
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("root exit was hidden by grandchild output handles")
	}
	job := recordMap(readJob(dir, hostTestID))
	if job["state"] != "SUCCEEDED" {
		t.Fatalf("completion: %#v", job)
	}
	awaitHostLeafStopped(t, pid)
}

func TestHostCheckpointFailureStopsTree(t *testing.T) {
	root := jobsScratchDir(t, "host-checkpoint-")
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path, pidPath := filepath.Join(dir, hostTestID+".json"), filepath.Join(root, "leaf")
	t.Setenv("PURSER_HOST_FAKE_WORKER", "1")
	t.Setenv("PURSER_HOST_FAKE_MODE", "tree-wait")
	t.Setenv("PURSER_HOST_TREE_PID", pidPath)
	writeHostTestJSON(t, path, map[string]any{"jobId": hostTestID, "state": "QUEUED", "generation": 1, "ownershipToken": "owner", "signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{"name": "codex", "args": map[string]any{"cwd": root}})
	original := replaceFile
	defer func() { replaceFile = original }()
	replaceFile = func(old, new string) error {
		if new == path {
			if candidate, err := Read(old); err == nil {
				if _, checkpoint := candidate.values["liveness"]; checkpoint {
					until := time.Now().Add(3 * time.Second)
					for time.Now().Before(until) {
						if _, err := os.Stat(pidPath); err == nil {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					return errors.New("checkpoint denied")
				}
			}
		}
		return original(old, new)
	}
	done := make(chan error, 1)
	go func() {
		_, err := RunHostGeneration(dir, hostTestID, 1, "owner", HostOptions{WorkerCommand: func(args LaunchArgs) ([]string, error) {
			return []string{os.Args[0], "-test.run=TestHostFakeWorker", "--", args.OutputPath}, nil
		}})
		done <- err
	}()
	pid := awaitHostLeaf(t, pidPath)
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
			_ = p.Release()
		}
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "checkpoint denied") {
			t.Fatalf("checkpoint failure lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint failure did not stop the tree")
	}
	awaitHostLeafStopped(t, pid)
	if stopped, err := recordedTreeStopped(dir, readJob(dir, hostTestID)); !stopped {
		t.Fatalf("checkpoint tree unconfirmed: %v", err)
	}
}

func awaitHostLeaf(t *testing.T, path string) int {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		data, _ := os.ReadFile(path)
		var pid int
		if json.Unmarshal(data, &pid) == nil && pid > 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker was not admitted")
	return 0
}

func awaitHostLeafStopped(t *testing.T, pid int) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if _, ok := hostLeafIdentity(pid); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant %d survived", pid)
}

func TestRecoverWaitsForConfirmedSupervision(t *testing.T) {
	root := jobsScratchDir(t, "recover-supervision-")
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(dir, hostTestID+".tree-test")
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "RUNNING", "generation": 1, "ownershipToken": "owner",
		"hostPid": 123, "hostCreatedAt": 456, "hostContained": true, "pid": 789,
		"treeReceipt": receipt,
	})
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".launch"), map[string]any{"name": "codex"})
	opt := RecoverOptions{ProcessAlive: func(int, int64) string { return "DEAD" },
		GroupStopped: func(int) bool { return true },
		Replace:      func(string, string, *Record, float64) string { return "REPLACED" }}
	writeHostTestJSON(t, receipt, map[string]any{"confirmed": false, "error": "Access is denied."})
	if got := RecoverJob(root, hostTestID, 1, opt); got != "UNCONFIRMED" {
		t.Fatalf("denied teardown admitted recovery: %s", got)
	}
	writeHostTestJSON(t, receipt, map[string]any{"confirmed": true})
	if got := RecoverJob(root, hostTestID, 1, opt); got != "REPLACED" {
		t.Fatalf("confirmed teardown blocked recovery: %s", got)
	}
}

func TestCancelRetainsUnconfirmedTeardownError(t *testing.T) {
	root := jobsScratchDir(t, "cancel-supervision-")
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(dir, hostTestID+".tree-test")
	writeHostTestJSON(t, filepath.Join(dir, hostTestID+".json"), map[string]any{
		"jobId": hostTestID, "state": "UNKNOWN_OR_IN_PROGRESS", "treeReceipt": receipt,
	})
	writeHostTestJSON(t, receipt, map[string]any{"confirmed": false, "error": "Access is denied."})
	got := Cancel(root, map[string]any{"jobId": hostTestID})["structuredContent"].(map[string]any)
	if got["errorCode"] != "PROCESS_UNCONFIRMED" || got["message"] != "Access is denied." {
		t.Fatalf("uncertain teardown was discarded or error lost: %#v", got)
	}
	worktree := filepath.Join(root, "wt", "blocked")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	job := readJob(dir, hostTestID)
	_ = job.Set("state", "FAILED")
	_ = job.Set("applyState", "rejected")
	_ = job.Set("worktreePath", worktree)
	if err := Write(filepath.Join(dir, hostTestID+".json"), job); err != nil {
		t.Fatal(err)
	}
	if ok, err := retention.AttachWorktree(root, worktree, hostTestID, ""); err != nil || !ok {
		t.Fatalf("attach: %v %v", ok, err)
	}
	if worktreeRetirableNow(root, worktree) {
		t.Fatal("denied teardown authorized retirement")
	}
	if _, err := retention.MarkWorktreeRetiring(root, worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.UpdateWorktreeOwnership(root, worktree, func(r *retention.WorktreeOwnership) { r.Claimed = true }); err != nil {
		t.Fatal(err)
	}
	if err := RetireWorktree(root, hostTestID); err == nil || err.Error() != "Access is denied." {
		t.Fatalf("direct retirement: %v", err)
	}
}

func TestValidationRecoveryWaitsForTreeStop(t *testing.T) {
	root := jobsScratchDir(t, "validation-recovery-")
	key := strings.Repeat("a", 64)
	path := validationRecordPath(root, key, hostTestID)
	receipt := filepath.Join(filepath.Dir(path), hostTestID+".tree-test")
	r := applyValidation{JobID: hostTestID, Status: "running", TreeReceipt: receipt}
	if err := writeApplyValidation(path, r); err != nil {
		t.Fatal(err)
	}
	if err := writeTreeReceipt(receipt, false, errors.New("Access is denied.")); err != nil {
		t.Fatal(err)
	}
	RecoverApplyValidations(root)
	r, err := readApplyValidation(path)
	if err != nil || r.Status != "unconfirmed" || r.Reason != "Access is denied." {
		t.Fatalf("denied recovery: %+v %v", r, err)
	}
	if err := writeTreeReceipt(receipt, true, nil); err != nil {
		t.Fatal(err)
	}
	RecoverApplyValidations(root)
	r, err = readApplyValidation(path)
	if err != nil || r.Status != "interrupted" {
		t.Fatalf("confirmed recovery: %+v %v", r, err)
	}
}
