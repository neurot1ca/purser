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

func fakeBridge(t *testing.T, body string) (string, string) {
	t.Helper()
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Fatal("Python required for fake bridge script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake_bridge.py")
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return python, script
}

func TestRunBridgeOutcomes(t *testing.T) {
	for _, outcome := range []string{"completed", "unavailable", "incomplete"} {
		t.Run(outcome, func(t *testing.T) {
			python, script := fakeBridge(t, "import json, sys\nsys.stdin.read()\nprint(json.dumps({'outcome': '"+outcome+"', 'available': True}))\n")
			got, err := runBridge(context.Background(), python, script, map[string]any{"task_id": "t"}, time.Second)
			if err != nil || got.Outcome != outcome || got.Attributes["available"] != true {
				t.Fatalf("RunBridge = %#v, %v", got, err)
			}
		})
	}
}

func TestRunBridgeErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
		timeout          time.Duration
	}{
		{"nonzero", "import sys\nsys.exit(7)\n", "BRIDGE_PROCESS_FAILED", time.Second},
		{"empty", "pass\n", "BRIDGE_EMPTY_OUTPUT", time.Second},
		{"multiple", "print('{\"outcome\":\"completed\"}')\nprint('{\"outcome\":\"unavailable\"}')\n", "BRIDGE_MULTIPLE_OUTPUTS", time.Second},
		{"malformed", "print('{bad json}')\n", "BRIDGE_MALFORMED_JSON", time.Second},
		{"unknown outcome", "print('{\"outcome\":\"skipped\"}')\n", "BRIDGE_UNKNOWN_OUTCOME", time.Second},
		{"invalid input", "print('{\"error\":\"INVALID_INPUT\",\"message\":\"bad\"}')\nraise SystemExit(2)\n", "INVALID_INPUT", time.Second},
		{"timeout", "import time\ntime.sleep(10)\n", "BRIDGE_TIMEOUT", 40 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			python, script := fakeBridge(t, test.body)
			_, err := runBridge(context.Background(), python, script, map[string]any{}, test.timeout)
			var bridgeErr *BridgeError
			if !errors.As(err, &bridgeErr) || bridgeErr.Code != test.want {
				t.Fatalf("error = %v, want typed %s", err, test.want)
			}
		})
	}
}

func TestRunBridgeReviewerException(t *testing.T) {
	python, script := fakeBridge(t, "print('{\"outcome\":\"unavailable\",\"reason\":\"REVIEWER_BRIDGE_EXCEPTION\",\"error_code\":\"REVIEWER_BRIDGE_EXCEPTION\"}')\n")
	_, err := runBridge(context.Background(), python, script, map[string]any{}, time.Second)
	var bridgeErr *BridgeError
	if !errors.As(err, &bridgeErr) || !IsReviewerBridgeException(err) {
		t.Fatalf("reviewer exception = %v", err)
	}
}
