package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"purser-hook/internal/profile"
)

func TestRunWorktreeSetup(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	fixturePath := filepath.Join(repo, ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "worktree_setup.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name           string `json:"name"`
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeout_seconds"`
			Expected       struct {
				Failed     bool `json:"failed"`
				SetupError *struct {
					ExitCode *int   `json:"exitCode"`
					TimedOut bool   `json:"timedOut"`
					Output   string `json:"output"`
				} `json:"setupError,omitempty"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "Python ledger._run_worktree_setup" {
		t.Fatalf("unexpected oracle: %q", fixture.Oracle)
	}
	python := testutil.Python(t)
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			command := strings.Replace(test.Command, "python", `"`+python+`"`, 1)
			result := RunWorktreeSetup(t.TempDir(), profile.WorktreeSetup{
				Setup: &command, SetupTimeoutSeconds: test.TimeoutSeconds,
			})
			if test.Name == "missing_command" && test.Expected.SetupError != nil && test.Expected.SetupError.Output == "<nonempty-shell-error>" {
				if result.Failure == nil || result.Failure.Output == "" {
					t.Fatal("missing command did not produce shell error output")
				}
				result.Failure.Output = "<nonempty-shell-error>"
				if runtime.GOOS != "windows" {
					posixShellExitCode := 127
					test.Expected.SetupError.ExitCode = &posixShellExitCode
				}
			}
			got := struct {
				Failed     bool                `json:"failed"`
				SetupError *WorktreeSetupError `json:"setupError,omitempty"`
			}{Failed: result.Failed(), SetupError: result.Failure}
			wantJSON, _ := json.Marshal(test.Expected)
			gotJSON, _ := json.Marshal(got)
			var want, actual any
			_ = json.Unmarshal(wantJSON, &want)
			_ = json.Unmarshal(gotJSON, &actual)
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("setup result = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}
