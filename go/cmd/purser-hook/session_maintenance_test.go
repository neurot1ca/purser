package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"testing"

	"purser-hook/internal/contextio"
)

func saveSessionMaintenanceHooks(t *testing.T) {
	t.Helper()
	oldDiscover, oldCleanup := discoverSessionModelVersion, requestCleanup
	oldStart := startSessionMaintenance
	oldContext, oldWrite := contextSessionStart, writeSessionOutput
	t.Cleanup(func() {
		discoverSessionModelVersion, requestCleanup = oldDiscover, oldCleanup
		startSessionMaintenance = oldStart
		contextSessionStart, writeSessionOutput = oldContext, oldWrite
	})
}

func writeMaintenanceOptIn(t *testing.T, state, root string) {
	t.Helper()
	dir := filepath.Join(state, "projects", contextio.ProjectKey(root), "maintenance")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opt-in.json"), []byte(`{"enabled":true,"project_root":`+quoteJSON(root)+`}`), 0600); err != nil {
		t.Fatal(err)
	}
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestMaintenanceReportWakeRequiresMatchingOptIn(t *testing.T) {
	saveSessionMaintenanceHooks(t)
	state, root := t.TempDir(), t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	if output, err := testutil.Hide(exec.Command("git", "-C", root, "init", "-q")).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	startSessionMaintenance = func(args, env []string, _ string) error {
		starts++
		if len(args) != 5 || args[0] == "" || args[1] != "maintenance" || args[2] != "check" || args[3] != "--repo" || args[4] != resolved {
			t.Fatalf("spawn args = %#v", args)
		}
		for _, value := range env {
			if value == "PYTHONUTF8=1" {
				t.Fatal("Go maintenance launch must not set Python environment")
			}
		}
		return nil
	}
	wakeMaintenanceReport(root)
	if starts != 0 {
		t.Fatalf("without opt-in: starts=%d", starts)
	}
	writeMaintenanceOptIn(t, state, resolved)
	wakeMaintenanceReport(root)
	if starts != 1 {
		t.Fatalf("with opt-in: starts=%d", starts)
	}
}

func TestSessionMaintenanceCleansJobsWithoutRepository(t *testing.T) {
	saveSessionMaintenanceHooks(t)
	called := false
	t.Setenv("PURSER_STATE_DIR", t.TempDir())
	discoverSessionModelVersion = func(string) (string, string, bool) {
		t.Fatal("model discovery called without a repository")
		return "", "", false
	}
	requestCleanup = func(root string) error {
		called = true
		if root != stateRoot() {
			t.Fatalf("cleanup root = %q", root)
		}
		return nil
	}
	sessionMaintenance(map[string]any{})
	if !called {
		t.Fatal("cleanup was not called")
	}
}

func TestSessionStartOutputSurvivesMaintenancePanic(t *testing.T) {
	saveSessionMaintenanceHooks(t)
	t.Setenv("PURSER_STATE_DIR", t.TempDir())
	output := []byte("session context")
	contextSessionStart = func(map[string]any) []byte { return output }
	written := []byte(nil)
	writeSessionOutput = func(data []byte) { written = append(written, data...) }
	discoverSessionModelVersion = func(string) (string, string, bool) { panic("maintenance failure") }
	cleanupCalled := false
	requestCleanup = func(string) error { cleanupCalled = true; return nil }
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	handleSessionStart(map[string]any{"cwd": wd})
	if string(written) != string(output) || !cleanupCalled {
		t.Fatalf("output=%q cleanupCalled=%v", written, cleanupCalled)
	}
}
