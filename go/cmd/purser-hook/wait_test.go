package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/testutil"
)

func TestWaitReobserveProcess(t *testing.T) {
	root := os.Getenv("PURSER_WAIT_REOBSERVE_ROOT")
	if root == "" {
		return
	}
	if os.Getenv("PURSER_WAIT_EMPTY_EXIT") == "1" {
		os.Exit(255)
	}
	t.Setenv("PURSER_STATE_DIR", root)
	t.Setenv("PURSER_WAIT_MAX_SECONDS", "1")
	os.Exit(waitMain([]string{"samejob"}))
}

func TestWaitEmpty255ReobservesSameJob(t *testing.T) {
	scratch, err := filepath.Abs("../../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(scratch, "wait-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "samejob.json")
	job := map[string]any{"jobId": "samejob", "state": "RUNNING", "generation": 7,
		"ownershipToken": "owner", "signalToken": "signal", "deadlineAt": float64(time.Now().Add(time.Minute).UnixNano()) / 1e9}
	write := func() {
		data, _ := json.Marshal(job)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		empty bool
		state string
		code  int
		line  string
	}{
		{empty: true, state: "RUNNING", code: 255},
		{state: "RUNNING", code: 5, line: "purser-wait samejob STILL_WAITING"},
		{state: "SUCCEEDED", code: 0, line: "purser-wait samejob SUCCEEDED"},
	} {
		job["state"] = tc.state
		write()
		command := testutil.Hide(exec.Command(executable, "-test.run=^TestWaitReobserveProcess$"))
		command.Env = append(os.Environ(), "PURSER_WAIT_REOBSERVE_ROOT="+root, "PURSER_WAIT_EMPTY_EXIT=0")
		if tc.empty {
			command.Env[len(command.Env)-1] = "PURSER_WAIT_EMPTY_EXIT=1"
		}
		output, err := command.CombinedOutput()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != tc.code || strings.TrimSpace(string(output)) != tc.line {
			t.Fatalf("wait = %d/%q, want %d/%q", code, output, tc.code, tc.line)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var current map[string]any
		if err := json.Unmarshal(data, &current); err != nil {
			t.Fatal(err)
		}
		if current["state"] != tc.state || current["generation"] != float64(7) || current["ownershipToken"] != "owner" || current["capacityRetryAttempts"] != nil {
			t.Fatalf("waiter changed worker identity: %#v", current)
		}
	}
}
