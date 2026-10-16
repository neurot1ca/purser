package enforce

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// PostToolUseFailure dispatches each Codex failure event to its Go finalizer.
func PostToolUseFailure(event map[string]any, root string) (response map[string]any) {
	root = defaultPreStateRoot(root)
	tool := stringValue(event["tool_name"])
	if !codexTools[tool] {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			diagnostic := postFailureDiagnostic(event, fmt.Errorf("panic: %v", recovered))
			diagnostic["error"] = "RuntimeError: " + fmt.Sprint(recovered)
			_ = writeFailureDiagnostic(root, diagnostic)
			response = failureErrorOutput("RuntimeError: " + fmt.Sprint(recovered))
		}
	}()
	var context string
	var err error
	event, context, err = restoreMCPPostEvent(event, root)
	if err != nil {
		return failureErrorOutput(safeError(err))
	}
	defer func() { response = appendMCPPostContext(response, "PostToolUseFailure", context) }()
	if strings.HasSuffix(tool, "__codex_start") {
		return postFailureStart(event, root)
	}
	if strings.HasSuffix(tool, "__codex_result") || strings.HasSuffix(tool, "__codex_cancel") {
		return PostToolUseFailureAsyncFollowup(event, root)
	}
	return postSynchronousFailure(event, root)
}

func postSynchronousFailure(event map[string]any, root string) (response map[string]any) {
	var pending map[string]any
	var undeliveredPath string
	defer func() {
		if recovered := recover(); recovered != nil {
			if pending != nil {
				failureObservation(root, event, pending)
				failureObservationTail(root, pending)
			}
			diagnostic := postFailureDiagnostic(event, fmt.Errorf("panic: %v", recovered))
			diagnostic["error"] = "RuntimeError: " + fmt.Sprint(recovered)
			_ = writeFailureDiagnostic(root, diagnostic)
			response = failureErrorOutput("RuntimeError: " + fmt.Sprint(recovered))
		}
	}()
	failed := func(message string) map[string]any { return failureErrorOutput(message) }
	failedWithDiagnostic := func(err error) map[string]any {
		if root != "" {
			diagnostic := postFailureDiagnostic(event, err)
			diagnostic["error"] = failureErrorText(err)
			_ = writeFailureDiagnostic(root, diagnostic)
		}
		return failed(failureErrorText(err))
	}
	if root == "" {
		return failed("state root is missing")
	}
	key, err := callKey(event)
	if err != nil {
		return failedWithDiagnostic(err)
	}
	cleanupStaleFailurePending(root, event, key)
	var pendingFile string
	pending, pendingFile, err = readFailurePending(root, event, key)
	if err != nil {
		return failedWithDiagnostic(err)
	}
	undeliveredPath, _ = failureUndelivered(root, event, pending)
	projectKey := stringValue(pending["project_key"])
	if projectKey == "" {
		projectKey = "unresolved"
	}
	evidence := failureEvidence(event, pending)
	evidencePath, err := persistFailureEvidence(root, key, evidence)
	if err != nil {
		failureObservation(root, event, pending)
		return failedWithDiagnostic(err)
	}
	routingEnforced := pending["kind"] != "pretool_bypass"
	outcome := map[string]any{
		"status": "AMBIGUOUS_TOOL_FAILURE", "ambiguous_outcome": true,
		"work_performed": "UNKNOWN", "routing_enforced": routingEnforced,
		"evidence_path": evidencePath, "claude_review_mode": "FULL",
	}
	complete := stringValue(pending["task_id"]) != "" && stringValue(pending["route"]) != "" && stringValue(pending["model"]) != "" && stringValue(pending["effort"]) != ""
	if complete {
		err = saveFailureSession(root, event, pending, outcome)
	} else if routingEnforced {
		err = errors.New("normal pending call identity is incomplete")
	}
	if err == nil {
		if undeliveredPath != "" {
			failureUndeliveredTail(root, undeliveredPath, pendingFile, event, pending)
		} else {
			err = cleanupFailureTail(root, pendingFile, event, pending, false)
		}
	}
	recordFailedMechanicalAttempt(root, pending)
	failureObservation(root, event, pending)
	failureObservationTail(root, pending)
	if err != nil {
		return failedWithDiagnostic(err)
	}
	if undeliveredPath != "" {
		_ = os.Remove(undeliveredPath)
	}
	return failureSuccessOutput(evidencePath, routingEnforced)
}

func postFailureDiagnostic(event map[string]any, err error) map[string]any {
	diagnostic := postDiagnostic(event, err)
	diagnostic["phase"] = "PostToolUseFailure"
	return diagnostic
}

func readFailurePending(root string, event map[string]any, key string) (map[string]any, string, error) {
	path := filepath.Join(root, "pending", key+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		path = bypassPath(root, event)
		data, err = os.ReadFile(path)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, path, errors.New("pending call record is missing")
		}
		return nil, path, errors.New("pending call record is unreadable")
	}
	var pending map[string]any
	if json.Unmarshal(data, &pending) != nil || pending == nil {
		return nil, path, errors.New("pending call record is unreadable")
	}
	return pending, path, nil
}

func telemetryDuration(value any) any {
	var parsed float64
	switch number := value.(type) {
	case json.Number:
		var err error
		parsed, err = number.Float64()
		if err != nil {
			return nil
		}
	case float64:
		parsed = number
	case float32:
		parsed = float64(number)
	case int:
		parsed = float64(number)
	case int64:
		parsed = float64(number)
	default:
		return nil
	}
	if parsed < 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return nil
	}
	return parsed
}

func safeEventID(value string) string {
	value = strings.Map(func(char rune) rune {
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || strings.ContainsRune("._-", char) {
			return char
		}
		return '_'
	}, strings.Trim(value, ". "))
	if len(value) > 96 {
		value = value[:96]
	}
	if value == "" || value == ".." {
		return "unknown"
	}
	upper := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if upper == "CON" || upper == "PRN" || upper == "AUX" || upper == "NUL" ||
		(len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9') {
		value = "_" + value
	}
	return value
}
