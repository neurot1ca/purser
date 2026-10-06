package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/lease"
)

func TestLeasesList(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	store := lease.New(filepath.Join(state, "projects", "project-one"))
	if got := store.Acquire("owner", "task-one", []string{"src/é.go"}, lease.AcquireOptions{}, float64(time.Now().UnixNano())/1e9); got.Status != "OK" {
		t.Fatalf("acquire lease: %#v", got)
	}
	code, stdout, stderr := runLeasesForTest(t, "--list")
	if code != 0 || stderr != "" {
		t.Fatalf("leases --list = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "é.go") {
		t.Fatalf("list output escaped Unicode: %q", stdout)
	}
	var output map[string][]map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	rows := output["active"]
	if len(output) != 1 || len(rows) != 1 || rows[0]["project_key"] != "project-one" ||
		rows[0]["task_id"] != "task-one" || rows[0]["scope"] == nil ||
		rows[0]["age_seconds"] == nil || rows[0]["job_state"] != nil {
		t.Fatalf("list JSON shape = %#v", output)
	}
}

func TestLeasesRelease(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	for project, task := range map[string]string{"project-one": "target-task", "project-two": "other-task"} {
		store := lease.New(filepath.Join(state, "projects", project))
		if got := store.Acquire("owner", task, []string{"docs"}, lease.AcquireOptions{}, float64(time.Now().UnixNano())/1e9); got.Status != "OK" {
			t.Fatalf("acquire lease for %s: %#v", project, got)
		}
	}
	code, stdout, stderr := runLeasesForTest(t, "--release", "target-task")
	if code != 0 || stderr != "" {
		t.Fatalf("leases --release = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	var output map[string][]map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatal(err)
	}
	rows := output["released"]
	if len(output) != 1 || len(rows) != 1 || rows[0]["project_key"] != "project-one" ||
		rows[0]["task_id"] != "target-task" || rows[0]["scope"] == nil {
		t.Fatalf("release JSON shape = %#v", output)
	}
	if _, exists := rows[0]["job_id"]; !exists {
		t.Fatalf("release row is missing job_id: %#v", rows[0])
	}
}

func TestLeasesUnreadableRecord(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	path := filepath.Join(state, "projects", "project-one", "scope_leases.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runLeasesForTest(t, "--list")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "Lease record could not be read or updated: ") {
		t.Fatalf("unreadable lease result = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func runLeasesForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutWrite, stderrWrite
	code := leasesMain(args)
	os.Stdout, os.Stderr = oldStdout, oldStderr
	if err := stdoutWrite.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrWrite.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, err := io.ReadAll(stdoutRead)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(stderrRead)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(stdout), string(stderr)
}
