package review

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type BridgeOutcome struct {
	Outcome    string
	Attributes map[string]any
}

// BridgeError marks invalid input and reviewer bridge failures as non-passes.
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

// IsReviewerBridgeException preserves Post's special handling of unexpected reviewer failures.
func IsReviewerBridgeException(err error) bool {
	var bridgeErr *BridgeError
	return errors.As(err, &bridgeErr) && bridgeErr.Code == "REVIEWER_BRIDGE_EXCEPTION"
}

// RunBridge validates the bridge arguments and runs Reviewer-1 in Go.
func RunBridge(ctx context.Context, pluginRoot string, args map[string]any, timeout time.Duration) (outcome BridgeOutcome, err error) {
	if timeout <= 0 {
		return BridgeOutcome{}, &BridgeError{Code: "INVALID_TIMEOUT"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome, err = bridgePanic(recovered, args)
		}
	}()
	return nativeReview(ctx, pluginRoot, args)
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
