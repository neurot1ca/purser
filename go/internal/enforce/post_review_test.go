package enforce

import (
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"testing"
	"time"

	"purser-hook/internal/review"
)

func TestSchedulePostReviewOperationSkipsBeforeDeltaValidation(t *testing.T) {
	bundle := baseBundle("")
	bundle["delta"] = map[string]any{"status": "OK", "changed_file_count": 0, "changed_line_count": 0}
	schedulePostReview(t.TempDir(), map[string]any{"phase": "operation"}, bundle, postProfile{}, nil,
		map[string]any{}, time.Second, true, true)
	if bundle["reviewer1_skipped_reason"] != review.PhaseOperation || bundle["claude_review_mode"] != review.Full {
		t.Fatalf("operation review state = skipped:%v mode:%v", bundle["reviewer1_skipped_reason"], bundle["claude_review_mode"])
	}
	for _, finding := range bundle["findings"].([]any) {
		if finding.(map[string]any)["code"] == "IMPLEMENTATION_NO_DELTA" {
			t.Fatal("operation bundle has implementation no-delta finding")
		}
	}
}

func TestSchedulePostReviewImplementationNoDeltaForcesFull(t *testing.T) {
	bundle := baseBundle("")
	bundle["delta"] = map[string]any{"status": "OK", "changed_file_count": 0, "changed_line_count": 0}
	bundle["thread_id"] = "thread"
	bundle["worker_validation"] = map[string]any{"status": "PASS"}
	pending := map[string]any{"phase": "implementation", "route": "codex-main", "task_contract_grounded": true}
	schedulePostReview(t.TempDir(), pending, bundle, postProfile{}, nil,
		map[string]any{"small_delta_max_files": 10, "small_delta_max_lines": 20}, time.Second, true, true)
	if bundle["claude_review_mode"] != review.Full {
		t.Fatalf("implementation no-delta mode = %v, want FULL", bundle["claude_review_mode"])
	}
	found := false
	for _, raw := range bundle["findings"].([]any) {
		finding := raw.(map[string]any)
		if finding["code"] == "IMPLEMENTATION_NO_DELTA" {
			found = true
			if finding["message"] != "Implementation job produced no change in its worktree; check whether the worker wrote outside the tracked files." {
				t.Fatalf("implementation no-delta message = %v", finding["message"])
			}
		}
	}
	if !found {
		t.Fatal("implementation no-delta finding missing")
	}
}

func TestSchedulePostReviewImplementationDeltaUnchanged(t *testing.T) {
	bundle := baseBundle("")
	bundle["delta"] = map[string]any{"status": "OK", "changed_file_count": 1, "changed_line_count": 1}
	bundle["thread_id"] = "thread"
	bundle["worker_validation"] = map[string]any{"status": "PASS"}
	pending := map[string]any{"phase": "implementation", "route": "codex-main", "task_contract_grounded": true}
	schedulePostReview(t.TempDir(), pending, bundle, postProfile{}, nil,
		map[string]any{"small_delta_max_files": 10, "small_delta_max_lines": 20}, time.Second, true, true)
	if bundle["claude_review_mode"] != review.Audit {
		t.Fatalf("non-empty implementation mode = %v, want AUDIT", bundle["claude_review_mode"])
	}
	for _, raw := range bundle["findings"].([]any) {
		if raw.(map[string]any)["code"] == "IMPLEMENTATION_NO_DELTA" {
			t.Fatal("non-empty implementation bundle has implementation no-delta finding")
		}
	}
}

func anyList(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func postReviewRepo(t *testing.T, git, root string, delta map[string]any) {
	t.Helper()
	command := testutil.Hide(exec.Command(git, "init", "-q", root))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	paths, _ := postStringList(delta["changed_paths"])
	if len(paths) == 0 {
		paths = []string{"scope/file-0.txt", "scope/file-1.txt", "scope/file-2.txt"}
	}
	for _, name := range paths {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("worker change\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	command = testutil.Hide(exec.Command(git, "-C", root, "add", "--all"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	command = testutil.Hide(exec.Command(git, "-C", root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
}
