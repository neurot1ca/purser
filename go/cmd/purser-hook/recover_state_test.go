package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"purser-hook/internal/retention"
)

func recoveryFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	id := strings.Repeat("a", 64)
	path := filepath.Join(root, "preparations", id)
	for name, record := range map[string]any{
		filepath.Join(path, "state.json"):                         map[string]any{"state": "APPLIED"},
		filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"): map[string]any{"jobId": id, "state": "SUCCEEDED", "applyState": "applied"},
	} {
		recoveryWrite(t, name, record)
	}
	return root, path
}

func recoveryWrite(t *testing.T, path string, value any) {
	t.Helper()
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

func TestRecoverStateApplyOwnedAndRepeat(t *testing.T) {
	root, path := recoveryFixture(t)
	first, err := recoverState(root, true)
	if err != nil || first.ReclaimedBytes == 0 || len(first.Failures) != 0 {
		t.Fatalf("%+v, %v", first, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("residue remains: %v", err)
	}
	second, err := recoverState(root, true)
	if err != nil || second.ReclaimedBytes != 0 || len(second.Candidates) != 0 {
		t.Fatalf("repeat: %+v, %v", second, err)
	}
}

func TestRecoverStateDryRun(t *testing.T) {
	root, path := recoveryFixture(t)
	report, err := recoverState(root, false)
	if err != nil || len(report.Candidates) != 1 || report.ReclaimedBytes != 0 {
		t.Fatalf("%+v, %v", report, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverStateForgedOwnership(t *testing.T) {
	root := t.TempDir()
	foreign := filepath.Join(t.TempDir(), "foreign")
	recoveryWrite(t, foreign, map[string]any{"keep": true})
	recoveryWrite(t, filepath.Join(root, "worktree_ownership", "forged.json"), map[string]any{"worktreePath": foreign, "jobs": []string{strings.Repeat("a", 64)}, "retiring": true})
	report, err := recoverState(root, true)
	if err != nil || len(report.Candidates) != 1 || report.Candidates[0].Reason == "" || report.ReclaimedBytes != 0 {
		t.Fatalf("%+v, %v", report, err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverStateRechecksFinalizing(t *testing.T) {
	root, path := recoveryFixture(t)
	report, err := recoverState(root, false)
	if err != nil {
		t.Fatal(err)
	}
	recoveryWrite(t, filepath.Join(root, "async_jobs", strings.Repeat("a", 64)+".json"), map[string]any{"stage": "FINALIZING"})
	if err := applyRecoveryCandidate(root, &report.Candidates[0]); err == nil {
		t.Fatal("changed candidate removed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverStateMetadataFreeWorktree(t *testing.T) {
	root, prep := recoveryFixture(t)
	id := filepath.Base(prep)
	wt := filepath.Join(root, "wt", "residue")
	recoveryWrite(t, filepath.Join(wt, "payload"), map[string]any{"owned": true})
	if ok, err := retention.AttachWorktree(root, wt, id, ""); err != nil || !ok {
		t.Fatalf("attach: %v, %v", ok, err)
	}
	first, err := recoverState(root, true)
	if err != nil || len(first.Failures) != 0 || first.ReclaimedBytes == 0 {
		t.Fatalf("%+v, %v", first, err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree retained: %v", err)
	}
	owned, err := retention.ReadWorktreeOwnership(root, wt)
	if err != nil || !owned.WorktreeGone {
		t.Fatalf("recovery journal lost: %+v, %v", owned, err)
	}
	second, err := recoverState(root, true)
	if err != nil || second.ReclaimedBytes != 0 || len(second.Candidates) != 0 {
		t.Fatalf("repeat: %+v, %v", second, err)
	}
}

func TestRecoverStateRetainsLinkAndAmbiguity(t *testing.T) {
	for _, kind := range []string{"link", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			root, path := recoveryFixture(t)
			if kind == "link" {
				external := t.TempDir()
				recoveryWrite(t, filepath.Join(external, "keep.json"), true)
				if err := os.Symlink(external, filepath.Join(path, "link")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				recoveryWrite(t, filepath.Join(path, "state.json"), map[string]any{})
			}
			report, err := recoverState(root, true)
			if err != nil || report.ReclaimedBytes != 0 || len(report.Candidates) != 1 || report.Candidates[0].Reason == "" {
				t.Fatalf("%+v, %v", report, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoverStateSharingFailureKeepsAuthorityAndResumes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows sharing violation")
	}
	root, path := recoveryFixture(t)
	payload := filepath.Join(path, "payload")
	recoveryWrite(t, payload, true)
	file, err := os.Open(payload)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	first, err := recoverState(root, true)
	if err != nil || first.ReclaimedBytes != 0 || len(first.Failures) != 1 || first.Candidates[0].Reason == "" {
		t.Fatalf("%+v, %v", first, err)
	}
	if _, err := os.Stat(filepath.Join(path, "state.json")); err != nil {
		t.Fatalf("lost retry authority: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := recoverState(root, true)
	if err != nil || second.ReclaimedBytes == 0 || len(second.Failures) != 0 {
		t.Fatalf("resume: %+v, %v", second, err)
	}
}

func TestRecoverStateLongReadOnlyTree(t *testing.T) {
	root, path := recoveryFixture(t)
	nested := filepath.Join(path, strings.Repeat("n", 100), strings.Repeat("m", 100), "payload")
	recoveryWrite(t, nested, true)
	if err := os.Chmod(nested, 0400); err != nil {
		t.Fatal(err)
	}
	report, err := recoverState(root, true)
	if err != nil || report.ReclaimedBytes == 0 || len(report.Failures) != 0 {
		t.Fatalf("%+v, %v", report, err)
	}
}
