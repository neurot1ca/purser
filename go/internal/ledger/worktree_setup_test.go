package ledger

import (
	"runtime"
	"testing"

	"purser-hook/internal/profile"
)

func TestRunWorktreeSetup(t *testing.T) {
	success, failure, timeout := "printf 'setup ok\\n'", "printf 'setup failed\\n'; exit 7", "printf 'started\\n'; sleep 5"
	if runtime.GOOS == "windows" {
		success = "echo setup ok"
		failure = "echo setup failed & exit /b 7"
		timeout = "echo started & ping -n 6 127.0.0.1 >nul 2>&1"
	}
	tests := []struct {
		name    string
		command string
		timeout int
		check   func(*testing.T, WorktreeSetupResult)
	}{
		{name: "success", command: success, timeout: 5, check: func(t *testing.T, got WorktreeSetupResult) {
			if got.Failed() {
				t.Fatalf("setup failed: %#v", got.Failure)
			}
		}},
		{name: "nonzero_exit", command: failure, timeout: 5, check: func(t *testing.T, got WorktreeSetupResult) {
			want := "setup failed\n"
			if runtime.GOOS == "windows" {
				want = "setup failed \n"
			}
			if got.Failure == nil || got.Failure.ExitCode == nil || *got.Failure.ExitCode != 7 || got.Failure.TimedOut || got.Failure.Output != want {
				t.Fatalf("setup failure = %#v", got.Failure)
			}
		}},
		{name: "timeout", command: timeout, timeout: 1, check: func(t *testing.T, got WorktreeSetupResult) {
			want := "started\n"
			if runtime.GOOS == "windows" {
				want = "started \n"
			}
			if got.Failure == nil || !got.Failure.TimedOut || got.Failure.ExitCode != nil || got.Failure.Output != want {
				t.Fatalf("setup timeout = %#v", got.Failure)
			}
		}},
		{name: "missing_command", command: "purser_missing_setup_command_76136", timeout: 5, check: func(t *testing.T, got WorktreeSetupResult) {
			if got.Failure == nil || got.Failure.TimedOut || got.Failure.Output == "" {
				t.Fatalf("missing command result = %#v", got.Failure)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := RunWorktreeSetup(t.TempDir(), profile.WorktreeSetup{Setup: &test.command, SetupTimeoutSeconds: test.timeout})
			test.check(t, got)
		})
	}
}
