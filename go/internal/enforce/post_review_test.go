package enforce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"testing"
	"time"

	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

func TestSchedulePostReviewPythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_review.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name            string          `json:"name"`
			Pending         map[string]any  `json:"pending"`
			Profile         postProfile     `json:"profile"`
			Delta           map[string]any  `json:"delta"`
			BundleForceFull bool            `json:"bundle_force_full"`
			Checks          []profile.Check `json:"checks"`
			GateConfig      map[string]any  `json:"gate_config"`
			RouteKnown      bool            `json:"route_known"`
			Expected        map[string]any  `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root := t.TempDir()
			postReviewRepo(t, root, test.Delta)
			bundle := baseBundle("")
			bundle["delta"], bundle["thread_id"], bundle["worker_validation"] = test.Delta, "fixture-thread", map[string]any{"status": "PASS", "summary": "fixture"}
			if paths, ok := postStringList(test.Delta["changed_paths"]); ok {
				protected := []string{}
				for _, path := range paths {
					if profile.IsProtected(path, test.Profile.ProtectedPaths) {
						protected = append(protected, path)
					}
				}
				bundle["protected_paths_touched"] = protected
			}
			if test.BundleForceFull || test.Pending["force_full_review"] == true {
				bundle["force_full_review"] = true
			}
			schedulePostReview(root, test.Pending, bundle, test.Profile, test.Checks,
				test.GateConfig, 10*time.Second, true, test.RouteKnown)
			report, _ := bundle["hook_checks"].(map[string]any)
			actual := map[string]any{"claude_review_mode": bundle["claude_review_mode"], "reviewer1_skipped_reason": bundle["reviewer1_skipped_reason"],
				"hook_status": "NOT_RUN", "side_effect_paths": []any{}, "findings_codes": []any{}}
			if report != nil {
				actual["hook_status"] = report["status"]
				if full, ok := bundle["hook_checks_full"].(review.ChecksReport); ok {
					actual["side_effect_paths"] = anyList(full.SideEffectPaths)
				}
			}
			findings, _ := bundle["findings"].([]any)
			codes := []any{}
			for _, raw := range findings {
				if finding, ok := raw.(map[string]any); ok {
					codes = append(codes, finding["code"])
				}
			}
			actual["findings_codes"] = codes
			if !reflect.DeepEqual(actual, test.Expected) {
				t.Fatalf("post review = %#v, want Python oracle %#v", actual, test.Expected)
			}
		})
	}
}

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

func postReviewRepo(t *testing.T, root string, delta map[string]any) {
	t.Helper()
	command := testutil.Hide(exec.Command("git", "init", "-q", root))
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
	command = testutil.Hide(exec.Command("git", "-C", root, "add", "--all"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	command = testutil.Hide(exec.Command("git", "-C", root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
}
