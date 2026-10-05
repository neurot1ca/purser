package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	"purser-hook/internal/proc"
)

type BridgeOutcome struct {
	Outcome    string
	Attributes map[string]any
}

// BridgeError marks every launch or protocol failure as a non-pass.
type BridgeError struct {
	Code string
	Err  error
}

func (e *BridgeError) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *BridgeError) Unwrap() error { return e.Err }

// IsReviewerBridgeException identifies the Python bridge's reviewer-exception reply.
// Python Post maps a raising reviewer to INTERNAL_POST_ERROR, not REVIEWER1_UNAVAILABLE.
func IsReviewerBridgeException(err error) bool {
	var bridgeErr *BridgeError
	return errors.As(err, &bridgeErr) && bridgeErr.Code == "REVIEWER_BRIDGE_EXCEPTION"
}

// RunBridge invokes reviewer_bridge.py through the shared Python locator.
func RunBridge(ctx context.Context, pluginRoot string, args map[string]any, timeout time.Duration) (BridgeOutcome, error) {
	if timeout <= 0 {
		return BridgeOutcome{}, &BridgeError{Code: "INVALID_TIMEOUT"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	python := proc.ResolvePython(ctx, pluginRoot)
	if ctx.Err() != nil {
		return BridgeOutcome{}, bridgeContextError(ctx.Err())
	}
	if python == "" {
		return BridgeOutcome{}, &BridgeError{Code: "PYTHON_UNAVAILABLE"}
	}
	script := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "reviewer_bridge.py")
	deadline, _ := ctx.Deadline()
	return runBridge(ctx, python, script, args, time.Until(deadline))
}

func runBridge(parent context.Context, python, script string, args map[string]any, timeout time.Duration) (BridgeOutcome, error) {
	input, err := json.Marshal(args)
	if err != nil {
		return BridgeOutcome{}, &BridgeError{Code: "INVALID_INPUT", Err: err}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, script)
	proc.ConfigureProcessTreeKill(cmd)
	cmd.Stdin = bytes.NewReader(input)
	output, runErr := cmd.Output()
	if ctx.Err() != nil {
		return BridgeOutcome{}, bridgeContextError(ctx.Err())
	}
	if runErr != nil {
		if bridgeInvalidInput(output) {
			return BridgeOutcome{}, &BridgeError{Code: "INVALID_INPUT", Err: runErr}
		}
		return BridgeOutcome{}, &BridgeError{Code: "BRIDGE_PROCESS_FAILED", Err: runErr}
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return BridgeOutcome{}, &BridgeError{Code: "BRIDGE_EMPTY_OUTPUT"}
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var attributes map[string]any
	if err := decoder.Decode(&attributes); err != nil || attributes == nil {
		return BridgeOutcome{}, &BridgeError{Code: "BRIDGE_MALFORMED_JSON", Err: err}
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return BridgeOutcome{}, &BridgeError{Code: "BRIDGE_MULTIPLE_OUTPUTS"}
	} else if !errors.Is(err, io.EOF) {
		return BridgeOutcome{}, &BridgeError{Code: "BRIDGE_MALFORMED_JSON", Err: err}
	}
	if attributes["error"] == "INVALID_INPUT" {
		return BridgeOutcome{}, &BridgeError{Code: "INVALID_INPUT"}
	}
	outcome, ok := attributes["outcome"].(string)
	if !ok || (outcome != "completed" && outcome != "unavailable" && outcome != "incomplete") {
		return BridgeOutcome{}, &BridgeError{Code: "BRIDGE_UNKNOWN_OUTCOME"}
	}
	if attributes["reason"] == "REVIEWER_BRIDGE_EXCEPTION" || attributes["error_code"] == "REVIEWER_BRIDGE_EXCEPTION" {
		return BridgeOutcome{}, &BridgeError{Code: "REVIEWER_BRIDGE_EXCEPTION"}
	}
	return BridgeOutcome{Outcome: outcome, Attributes: attributes}, nil
}

func bridgeContextError(err error) *BridgeError {
	code := "BRIDGE_CANCELED"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "BRIDGE_TIMEOUT"
	}
	return &BridgeError{Code: code, Err: err}
}

func bridgeInvalidInput(output []byte) bool {
	var reply map[string]any
	return json.Unmarshal(output, &reply) == nil && reply["error"] == "INVALID_INPUT"
}
