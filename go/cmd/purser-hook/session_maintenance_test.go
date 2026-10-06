package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func saveSessionMaintenanceHooks(t *testing.T) {
	t.Helper()
	oldDiscover, oldCleanup := discoverSessionModelVersion, cleanupSessionJobs
	oldResolve, oldStart, oldStderr := resolveSessionPython, startSessionMaintenance, sessionMaintenanceStderr
	oldContext, oldWrite := contextSessionStart, writeSessionOutput
	t.Cleanup(func() {
		discoverSessionModelVersion, cleanupSessionJobs = oldDiscover, oldCleanup
		resolveSessionPython, startSessionMaintenance, sessionMaintenanceStderr = oldResolve, oldStart, oldStderr
		contextSessionStart, writeSessionOutput = oldContext, oldWrite
	})
}

func writeMaintenanceOptIn(t *testing.T, state, root string) {
	t.Helper()
	dir := filepath.Join(state, "projects", maintenanceProjectKey(root), "maintenance")
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
	state, root, plugin := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	writes, resolves, starts := 0, 0, 0
	resolveSessionPython = func(context.Context, string) string { resolves++; return "python" }
	startSessionMaintenance = func(args, env []string, _ string) error {
		starts++
		if len(args) != 5 || args[0] != "python" || args[1] != filepath.Join(plugin, ".claude", "hooks", "orchestrator", "maintenance_reports.py") || args[2] != "check" || args[3] != "--repo" || args[4] != resolved {
			t.Fatalf("spawn args = %#v", args)
		}
		foundUTF8 := false
		for _, value := range env {
			if value == "PYTHONUTF8=1" {
				foundUTF8 = true
			}
		}
		if !foundUTF8 {
			t.Fatal("spawn environment lacks PYTHONUTF8=1")
		}
		return nil
	}
	sessionMaintenanceStderr = func(string) { writes++ }
	wakeMaintenanceReport(root)
	if resolves != 0 || starts != 0 || writes != 0 {
		t.Fatalf("without opt-in: resolves=%d starts=%d stderr=%d", resolves, starts, writes)
	}
	writeMaintenanceOptIn(t, state, resolved)
	wakeMaintenanceReport(root)
	if resolves != 1 || starts != 1 || writes != 0 {
		t.Fatalf("with opt-in: resolves=%d starts=%d stderr=%d", resolves, starts, writes)
	}
}

func TestMaintenanceReportPythonMissingWritesSkipLine(t *testing.T) {
	saveSessionMaintenanceHooks(t)
	state, root := t.TempDir(), t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	writeMaintenanceOptIn(t, state, resolved)
	resolveSessionPython = func(context.Context, string) string { return "" }
	starts := 0
	startSessionMaintenance = func([]string, []string, string) error { starts++; return nil }
	var lines []string
	sessionMaintenanceStderr = func(line string) { lines = append(lines, line) }
	wakeMaintenanceReport(root)
	if starts != 0 || !reflect.DeepEqual(lines, []string{"Purser: maintenance reports need Python; skipped"}) {
		t.Fatalf("starts=%d stderr=%q", starts, lines)
	}
}

func TestSessionMaintenanceCleansJobsWithoutRepository(t *testing.T) {
	saveSessionMaintenanceHooks(t)
	called := false
	discoverSessionModelVersion = func(string) (string, string, bool) {
		t.Fatal("model discovery called without a repository")
		return "", "", false
	}
	cleanupSessionJobs = func(root string, now time.Time, checkCeiling bool) {
		called = true
		if root != stateRoot() || time.Since(now) > time.Second || !checkCeiling {
			t.Fatalf("cleanup args = %q, %v, %v", root, now, checkCeiling)
		}
	}
	sessionMaintenance(map[string]any{})
	if !called {
		t.Fatal("cleanup was not called")
	}
}

func TestSessionStartOutputSurvivesMaintenancePanic(t *testing.T) {
	saveSessionMaintenanceHooks(t)
	output := []byte("session context")
	contextSessionStart = func(map[string]any) []byte { return output }
	written := []byte(nil)
	writeSessionOutput = func(data []byte) { written = append(written, data...) }
	discoverSessionModelVersion = func(string) (string, string, bool) { panic("maintenance failure") }
	cleanupCalled := false
	cleanupSessionJobs = func(string, time.Time, bool) { cleanupCalled = true }
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	handleSessionStart(map[string]any{"cwd": wd})
	if string(written) != string(output) || !cleanupCalled {
		t.Fatalf("output=%q cleanupCalled=%v", written, cleanupCalled)
	}
}
