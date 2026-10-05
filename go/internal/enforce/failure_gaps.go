package enforce

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/lease"
)

// failureUndelivered overlays the exceptional rewrite marker onto the failed
// call so evidence and telemetry retain Python's bypass attribution.
func failureUndelivered(root string, event, pending map[string]any) (string, bool) {
	path, marker, found := readPostUndelivered(root, event)
	if !found {
		return "", false
	}
	reason := stringValue(marker["reason"])
	if reason == "" {
		reason = "adapter failed before rewritten input was delivered"
	}
	pending["kind"], pending["reason"] = "pretool_bypass", reason
	return path, true
}

// failureUndeliveredTail performs best-effort cleanup and observations after
// an undelivered marker has been attributed to a failed call.
func failureUndeliveredTail(root, markerPath, pendingFile string, event, pending map[string]any) {
	project := pendingProjectKey(pending)
	if owner := stringValue(pending["lease_owner"]); owner != "" {
		_ = lease.New(filepath.Join(root, "projects", project)).Release(owner, float64(time.Now().UnixNano())/1e9)
	}
	_ = os.Remove(pendingFile)
	preEmit(root, event, nil, stringValue(pending["task_id"]), project,
		"ROUTING_PROTOCOL_VIOLATION", map[string]any{"violation_code": "CODEX_INPUT_REWRITE_UNDELIVERED"})
	_ = os.Remove(markerPath)
	updateObservationTaskBestEffort(filepath.Join(root, "telemetry"), stringValue(pending["task_id"]), project)
}

// recordFailedMechanicalAttempt mirrors Python's negative sample on failure.
// Mechanical state is optional and must never change the failure response.
func recordFailedMechanicalAttempt(root string, pending map[string]any) {
	if pending["mechanical_opencode"] != true || pending["agent"] != "opencode" {
		return
	}
	_ = recordMechanicalSample(root, stringValue(pending["model"]), false)
}

// failureInternalResponse preserves the standard FULL bundle and diagnostic
// when a failure-path operation itself raises. The failure dispatcher owns
// when to invoke this helper.
func failureInternalResponse(root string, event, pending map[string]any, cause error) map[string]any {
	bundle := baseBundle(safeError(cause))
	if pending != nil {
		bundle["task_id"], bundle["phase"] = pending["task_id"], pending["phase"]
		bundle["route"], bundle["model"], bundle["effort"] = pending["route"], pending["model"], pending["effort"]
		bundle["routing_enforced"] = pending["kind"] != "pretool_bypass"
	}
	if root != "" {
		diagnostic := postDiagnostic(event, cause)
		diagnostic["phase"] = "PostToolUseFailure"
		if err := writeFailureDiagnostic(root, diagnostic); err != nil {
			_, _ = os.Stderr.Write(append(diagnosticJSON(diagnostic), '\n'))
		}
	}
	response, err := replacement(bundle, "updatedToolOutput")
	if err == nil {
		mapValue(response["hookSpecificOutput"])["hookEventName"] = "PostToolUseFailure"
		return response
	}
	return failureErrorOutput(fmt.Sprintf("%s: %s", safeError(cause), err))
}

func writeFailureDiagnostic(root string, diagnostic map[string]any) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, "diagnostics.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(diagnosticJSON(diagnostic), '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// failureObservationTail adds Python's failed task projection refresh after
// the append-only failure observation. Projection errors are observational.
func failureObservationTail(root string, pending map[string]any) {
	if taskID := stringValue(pending["task_id"]); taskID != "" {
		updateObservationTaskBestEffort(filepath.Join(root, "telemetry"), taskID, pendingProjectKey(pending))
	}
}
