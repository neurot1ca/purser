package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
)

func TestQueuedBoardNamesUnlabelledBlocker(t *testing.T) {
	root := t.TempDir()
	project := "project-one"
	id := strings.Repeat("a", 64)
	store := lease.New(filepath.Join(root, "projects", project))
	if got := store.Acquire("running", "running-task", []string{"src/occupied"}, lease.AcquireOptions{}, 100); got.Status != "OK" {
		t.Fatal(got)
	}
	olderID := strings.Repeat("b", 64)
	if got := store.Acquire("older", "older-task", []string{"src"}, lease.AcquireOptions{JobID: &olderID, QueueConflict: true}, 101); got.Code != "LEASE_QUEUED" {
		t.Fatal(got)
	}
	if got := store.Acquire("queued", "queued-task", []string{"src/free"}, lease.AcquireOptions{JobID: &id, QueueConflict: true}, 102); got.Code != "LEASE_QUEUED" {
		t.Fatal(got)
	}
	for path, value := range map[string]any{
		filepath.Join(root, "async_jobs", id+".json"):             map[string]any{"jobId": id, "project_key": project, "call": "queued"},
		filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"): map[string]any{"jobId": id, "call": "queued", "state": "QUEUED", "label": "waiting-task"},
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	board, _ := enforce.PostProjectJobBoard(root, project, time.Unix(102, 0))
	if !strings.Contains(board, "queued behind older-task") {
		t.Fatalf("board does not name blocker: %s", board)
	}
}

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
