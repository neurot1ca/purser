package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func fakeReadinessBridge(t *testing.T, body string) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Fatal("Python required for fake readiness bridge")
	}
	script := filepath.Join(t.TempDir(), "fake_readiness.py")
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return python, script
}

func TestRunReadinessBridge(t *testing.T) {
	request := ReadinessRequest{Grounded: true, Objective: "objective", AcceptanceCriteria: "criterion",
		AttributedDiff: "diff", WorkerReport: "done", Agent: "agy", Model: "gemini-test", RepoRoot: "/repo"}
	tests := []struct {
		name, body, want string
		timeout          time.Duration
	}{
		{"success", "print('{\"verdict\":\"NO_UNMET_FOUND\",\"criterion\":null,\"evidence\":[]}')\n", "NO_UNMET_FOUND", time.Second},
		{"timeout", "import time\ntime.sleep(5)\n", "UNAVAILABLE", 1200 * time.Millisecond},
		{"malformed", "print('{bad json}')\n", "UNAVAILABLE", time.Second},
		{"nonzero", "import sys\nsys.exit(7)\n", "UNAVAILABLE", time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			python, script := fakeReadinessBridge(t, test.body)
			got, err := runReadiness(context.Background(), python, script, request, test.timeout)
			if got.Verdict != test.want {
				t.Fatalf("verdict = %#v, want %s (err %v)", got, test.want, err)
			}
			if test.name == "success" && err != nil {
				t.Fatalf("success error = %v", err)
			}
			if test.name != "success" {
				var bridgeErr *BridgeError
				if !errors.As(err, &bridgeErr) {
					t.Fatalf("error = %v, want typed bridge error", err)
				}
				if test.name == "timeout" && bridgeErr.Code != "BRIDGE_TIMEOUT" {
					t.Fatalf("timeout code = %s", bridgeErr.Code)
				}
			}
		})
	}
}
