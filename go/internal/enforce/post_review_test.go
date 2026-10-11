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
	schedulePostReview(t.TempDir(), map[string]any{"phase": "operation"}, bundle, postProfile{}, nil,
		map[string]any{}, time.Second, true, true)
	if bundle["reviewer1_skipped_reason"] != review.PhaseOperation || bundle["claude_review_mode"] != review.Full {
		t.Fatalf("operation review state = skipped:%v mode:%v", bundle["reviewer1_skipped_reason"], bundle["claude_review_mode"])
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
