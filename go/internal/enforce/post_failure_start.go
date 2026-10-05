package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// postFailureStart mirrors the codex_start failure branch without connecting
// it to the failure dispatcher while the Post lane is being ported.
func postFailureStart(event map[string]any, root string) map[string]any {
	tool := stringValue(event["tool_name"])
	if event["hook_event_name"] != "PostToolUseFailure" || !strings.HasSuffix(tool, "__codex_start") {
		return nil
	}
	if root == "" {
		return failureErrorOutput("state root is missing")
	}
	key, err := callKey(event)
	if err != nil {
		return failureErrorOutput(failureErrorText(err))
	}
	cleanupStaleFailurePending(root, event, key)
	pendingPath := filepath.Join(root, "pending", key+".json")
	pending, _, err := readFailurePending(root, event, key)
	if err != nil {
		return failureErrorOutput(failureErrorText(err))
	}
	call := stringValue(pending["async_call"])
	if call == "" {
		if input, ok := event["tool_input"].(map[string]any); ok {
			call = stringValue(input["call"])
		}
	}
	if call != "" {
		sum := sha256.Sum256([]byte(call))
		_ = cancelAsyncStart(root, call, hex.EncodeToString(sum[:]))
	}

	path, err := persistFailureEvidence(root, key, failureEvidence(event, pending))
	if err != nil {
		failureObservation(root, event, pending)
		return failureErrorOutput(failureErrorText(err))
	}
	routingEnforced := pending["kind"] != "pretool_bypass"
	outcome := map[string]any{"status": "AMBIGUOUS_TOOL_FAILURE", "ambiguous_outcome": true,
		"work_performed": "UNKNOWN", "routing_enforced": routingEnforced,
		"evidence_path": path, "claude_review_mode": "FULL"}
	complete := stringValue(pending["task_id"]) != "" && stringValue(pending["route"]) != "" &&
		stringValue(pending["model"]) != "" && stringValue(pending["effort"]) != ""
	if complete {
		err = saveFailureSession(root, event, pending, outcome)
	} else if routingEnforced {
		err = errors.New("normal pending call identity is incomplete")
	}
	if err != nil {
		failureObservation(root, event, pending)
		return failureErrorOutput(failureErrorText(err))
	}

	terminal := asyncStartTerminalOrAbsent(root, pending)
	if !terminal {
		pending["start_rejected"] = true
		encoded, encodeErr := canonicalJSON(pending)
		if encodeErr != nil {
			err = encodeErr
		} else {
			err = atomicObservationJSON(pendingPath, encoded)
		}
	}
	if err == nil && terminal {
		err = cleanupFailureTail(root, pendingPath, event, pending, false)
	}
	if err == nil {
		if jobID := stringValue(pending["job_id"]); asyncHash.MatchString(jobID) {
			indexPath := filepath.Join(root, "async_jobs", jobID+".json")
			removeIndex := true
			if index, readErr := readAsyncObject(indexPath); readErr == nil &&
				stringValue(index["project_key"]) != "" &&
				stringValue(index["project_key"]) != stringValue(pending["project_key"]) {
				removeIndex = false
			} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				removeIndex = false
			}
			if removeIndex {
				if removeErr := os.Remove(indexPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					err = removeErr
				}
			}
		}
	}
	failureObservation(root, event, pending)
	if err != nil {
		return failureErrorOutput(failureErrorText(err))
	}
	return failureSuccessOutput(path, routingEnforced)
}
