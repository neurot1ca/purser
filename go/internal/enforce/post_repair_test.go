package enforce

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPostRepairPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_repair.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string                           `json:"name"`
			Eligible bool                             `json:"eligible"`
			Outcome  string                           `json:"outcome"`
			Findings []struct{ Code, Message string } `json:"findings"`
			Expected map[string]any                   `json:"expected_result"`
			Capture  struct {
				Argv        []any `json:"argv"`
				CWD, Stdin  string
				Timeout     float64
				EnvSentinel string `json:"env_sentinel"`
			} `json:"capture"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POST_REPAIR_ENV_SENTINEL", "present")
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			findings := make([]postRepairFinding, len(tc.Findings))
			for i, f := range tc.Findings {
				findings[i] = postRepairFinding{Code: f.Code, Message: f.Message}
			}
			bundle := map[string]any{"micro_repair": nil, "micro_repair_cleared_review": false}
			var called bool
			runner := func(_ context.Context, argv, env []string, cwd, prompt string, timeout time.Duration) postRepairProcess {
				called = true
				wantArgv := make([]string, len(tc.Capture.Argv))
				for i, item := range tc.Capture.Argv {
					wantArgv[i], _ = item.(string)
				}
				if !reflect.DeepEqual(argv, wantArgv) || cwd != tc.Capture.CWD || prompt != tc.Capture.Stdin ||
					timeout.Seconds() != tc.Capture.Timeout || !envHas(env, "POST_REPAIR_ENV_SENTINEL=present") {
					t.Fatalf("process config argv=%v cwd=%q prompt-len=%d timeout=%v", argv, cwd, len(prompt), timeout)
				}
				switch tc.Outcome {
				case "timeout":
					return postRepairProcess{err: context.DeadlineExceeded, timedOut: true}
				case "nonzero":
					return postRepairProcess{err: errors.New("exit status 7"), stderr: "repair rejected"}
				case "malformed":
					return postRepairProcess{err: errors.New("AttributeError: 'object' object has no attribute 'returncode'")}
				default:
					return postRepairProcess{stdout: "repaired"}
				}
			}
			actual, attempted := attemptPostMicroRepair(bundle, map[bool]string{true: "codex", false: "agy"}[tc.Eligible], "CHANGES_REQUIRED", "thread-5", "fixture-repo", findings, []string{"src"}, true, 45, 60*time.Second, runner)
			if tc.Eligible != attempted || tc.Eligible != called {
				t.Fatalf("eligible=%v attempted=%v called=%v", tc.Eligible, attempted, called)
			}
			if tc.Eligible && !reflect.DeepEqual(actual, tc.Expected) {
				t.Fatalf("result=%#v want %#v", actual, tc.Expected)
			}
			if tc.Eligible {
				entry := bundle["micro_repair"].(map[string]any)
				wantStatus := map[string]string{"success": "RUNNING", "timeout": "TIMED_OUT", "nonzero": "FAILED", "malformed": "FAILED"}[tc.Outcome]
				if entry["status"] != wantStatus || bundle["micro_repair_cleared_review"] != false {
					t.Fatalf("repair bundle entry=%#v cleared=%v", entry, bundle["micro_repair_cleared_review"])
				}
			}
			if tc.Name == "prompt-bound" && len([]rune(tc.Capture.Stdin)) != len([]rune(postRepairPrompt(findings, []string{"src"}))) {
				t.Fatal("prompt bound mismatch")
			}
			if !tc.Eligible && (bundle["micro_repair"] != nil || bundle["micro_repair_cleared_review"] != false) {
				t.Fatalf("ineligible repair changed bundle: %#v", bundle)
			}
		})
	}
}

func envHas(env []string, want string) bool {
	for _, item := range env {
		if item == want {
			return true
		}
	}
	return false
}

func TestPostRepairPromptBound(t *testing.T) {
	findings := []postRepairFinding{{Code: "F", Message: strings.Repeat("é", 900)}}
	prompt := postRepairPrompt(findings, []string{"src"})
	bulletText := strings.SplitN(prompt, "issues in your previous diff:\n", 2)[1]
	bulletText = strings.SplitN(bulletText, "\nYou are strictly restricted", 2)[0]
	if len([]rune(bulletText)) != 500 {
		t.Fatalf("finding text has %d Python characters, want 500", len([]rune(bulletText)))
	}
}

func TestPostRepairOutputLimit(t *testing.T) {
	buffer := &cappedBuffer{limit: 4}
	if n, err := buffer.Write([]byte("123456")); n != 6 || err != nil || buffer.String() != "1234" {
		t.Fatalf("bounded output = %q, n=%d, err=%v", buffer.String(), n, err)
	}
}
