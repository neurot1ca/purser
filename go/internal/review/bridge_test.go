package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeBridge(t *testing.T, body string) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Fatal("Python required for fake bridge script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake_bridge.py")
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return python, script
}

func TestRunBridgeOutcomes(t *testing.T) {
	for _, outcome := range []string{"completed", "unavailable", "incomplete"} {
		t.Run(outcome, func(t *testing.T) {
			python, script := fakeBridge(t, "import json, sys\nsys.stdin.read()\nprint(json.dumps({'outcome': '"+outcome+"', 'available': True}))\n")
			got, err := runBridge(context.Background(), python, script, map[string]any{"task_id": "t"}, time.Second)
			if err != nil || got.Outcome != outcome || got.Attributes["available"] != true {
				t.Fatalf("RunBridge = %#v, %v", got, err)
			}
		})
	}
}

func TestRunBridgeErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
		timeout          time.Duration
	}{
		{"nonzero", "import sys\nsys.exit(7)\n", "BRIDGE_PROCESS_FAILED", time.Second},
		{"empty", "pass\n", "BRIDGE_EMPTY_OUTPUT", time.Second},
		{"multiple", "print('{\"outcome\":\"completed\"}')\nprint('{\"outcome\":\"unavailable\"}')\n", "BRIDGE_MULTIPLE_OUTPUTS", time.Second},
		{"malformed", "print('{bad json}')\n", "BRIDGE_MALFORMED_JSON", time.Second},
		{"unknown outcome", "print('{\"outcome\":\"skipped\"}')\n", "BRIDGE_UNKNOWN_OUTCOME", time.Second},
		{"invalid input", "print('{\"error\":\"INVALID_INPUT\",\"message\":\"bad\"}')\nraise SystemExit(2)\n", "INVALID_INPUT", time.Second},
		{"timeout", "import time\ntime.sleep(10)\n", "BRIDGE_TIMEOUT", 40 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			python, script := fakeBridge(t, test.body)
			_, err := runBridge(context.Background(), python, script, map[string]any{}, test.timeout)
			var bridgeErr *BridgeError
			if !errors.As(err, &bridgeErr) || bridgeErr.Code != test.want {
				t.Fatalf("error = %v, want typed %s", err, test.want)
			}
		})
	}
}

func TestRunBridgeReviewerException(t *testing.T) {
	python, script := fakeBridge(t, "print('{\"outcome\":\"unavailable\",\"reason\":\"REVIEWER_BRIDGE_EXCEPTION\",\"error_code\":\"REVIEWER_BRIDGE_EXCEPTION\"}')\n")
	_, err := runBridge(context.Background(), python, script, map[string]any{}, time.Second)
	var bridgeErr *BridgeError
	if !errors.As(err, &bridgeErr) || !IsReviewerBridgeException(err) {
		t.Fatalf("reviewer exception = %v", err)
	}
}

func TestReviewDoesNotImportEnforce(t *testing.T) {
	output, err := exec.Command("go", "list", "-f", "{{.Imports}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, output)
	}
	if strings.Contains(string(output), "purser-hook/internal/enforce") {
		t.Fatalf("review imports enforce: %s", output)
	}
}

func TestRunBridgeTimeoutKillsProcessTreeWithinBudget(t *testing.T) {
	assertBridgeTreeStopped(t, 300*time.Millisecond, false)
}

func TestRunBridgeCancellationKillsProcessTree(t *testing.T) {
	assertBridgeTreeStopped(t, 5*time.Second, true)
}

func assertBridgeTreeStopped(t *testing.T, budget time.Duration, cancelRun bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PURSER_BRIDGE_PARENT_PID", filepath.Join(dir, "parent.pid"))
	t.Setenv("PURSER_BRIDGE_CHILD_PID", filepath.Join(dir, "child.pid"))
	python, script := fakeBridge(t, "import os, subprocess, sys, time\n"+
		"open(os.environ['PURSER_BRIDGE_PARENT_PID'], 'w').write(str(os.getpid()))\n"+
		"child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)\n"+
		"open(os.environ['PURSER_BRIDGE_CHILD_PID'], 'w').write(str(child.pid))\n"+
		"time.sleep(60)\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		started := time.Now()
		_, err := runBridge(ctx, python, script, map[string]any{}, budget)
		done <- result{err: err, elapsed: time.Since(started)}
	}()
	parentPID := waitPIDFile(t, filepath.Join(dir, "parent.pid"))
	childPID := waitPIDFile(t, filepath.Join(dir, "child.pid"))
	defer func() {
		for _, pid := range []string{parentPID, childPID} {
			if process, err := os.FindProcess(mustPID(t, pid)); err == nil {
				_ = process.Kill()
			}
		}
	}()
	if cancelRun {
		cancel()
	}
	got := <-done
	wantCode := "BRIDGE_TIMEOUT"
	if cancelRun {
		wantCode = "BRIDGE_CANCELED"
	} else if got.elapsed < budget-100*time.Millisecond || got.elapsed > budget+500*time.Millisecond {
		t.Fatalf("bridge timeout budget %s, elapsed %s", budget, got.elapsed)
	}
	var bridgeErr *BridgeError
	if !errors.As(got.err, &bridgeErr) || bridgeErr.Code != wantCode {
		t.Fatalf("bridge error = %v, want %s", got.err, wantCode)
	}
	for _, process := range []struct{ role, pid string }{{"bridge", parentPID}, {"child", childPID}} {
		deadline := time.Now().Add(2 * time.Second)
		for processExists(process.pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if processExists(process.pid) {
			t.Fatalf("%s process %s remains after %s", process.role, process.pid, wantCode)
		}
	}
}

func waitPIDFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file not written: %s", path)
	return ""
}

func processExists(pid string) bool {
	if runtime.GOOS == "windows" {
		command := "$p=Get-Process -Id " + pid + " -ErrorAction SilentlyContinue; if ($p) { exit 0 }; exit 1"
		err := exec.Command("powershell.exe", "-NoProfile", "-Command", command).Run()
		if err == nil {
			return true
		}
		exit, ok := err.(*exec.ExitError)
		return !ok || exit.ExitCode() != 1
	}
	output, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
			return false
		}
		return true
	}
	state := strings.TrimSpace(string(output))
	return state != "" && state[0] != 'Z'
}

func mustPID(t *testing.T, pid string) int {
	t.Helper()
	parsed, err := strconv.Atoi(pid)
	if err != nil || parsed <= 0 {
		t.Fatalf("invalid process id %q", pid)
	}
	return parsed
}
