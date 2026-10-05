package enforce

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/lease"
)

// PostToolUseFailure handles only the synchronous Codex failure path. Async
// result/cancel/start follow-ups remain owned by Python.
func PostToolUseFailure(event map[string]any, root string) map[string]any {
	tool := stringValue(event["tool_name"])
	if !codexTools[tool] || strings.HasSuffix(tool, "__codex_start") ||
		strings.HasSuffix(tool, "__codex_result") || strings.HasSuffix(tool, "__codex_cancel") {
		return nil
	}
	ctx := map[string]any{"hookEventName": "PostToolUseFailure"}
	failed := func(message string) map[string]any {
		ctx["additionalContext"] = "Codex failed with an ambiguous outcome; work_performed is UNKNOWN and FULL Claude review is required. Evidence cleanup error: " + message
		return map[string]any{"hookSpecificOutput": ctx}
	}
	if root == "" {
		return failed("state root is missing")
	}
	key, err := callKey(event)
	if err != nil {
		return failed(safeError(err))
	}
	pending, pendingFile, err := readFailurePending(root, event, key)
	if err != nil {
		return failed(safeError(err))
	}
	projectKey := stringValue(pending["project_key"])
	if projectKey == "" {
		projectKey = "unresolved"
	}
	evidence := failureEvidence(event, pending)
	evidencePath, err := persistFailureEvidence(root, key, evidence)
	if err != nil {
		failureObservation(root, event, pending)
		return failed(safeError(err))
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
		if owner := stringValue(pending["lease_owner"]); owner != "" {
			result := lease.New(filepath.Join(root, "projects", projectKey)).Release(owner, float64(time.Now().UnixNano())/1e9)
			if result.Status != "OK" {
				err = fmt.Errorf("scope lease cleanup failed: %s", result.Code)
			}
		}
	}
	if err == nil {
		if removeErr := os.Remove(pendingFile); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = fmt.Errorf("pending cleanup failed: %w", removeErr)
		}
	}
	failureObservation(root, event, pending)
	if err != nil {
		return failed(safeError(err))
	}
	note := ""
	if !routingEnforced {
		note = " Routing was NOT enforced by PreToolUse."
	}
	ctx["additionalContext"] = fmt.Sprintf("Codex failed with an ambiguous outcome; work_performed is UNKNOWN, FULL Claude review is required, and evidence is at %s.%s", evidencePath, note)
	return map[string]any{"hookSpecificOutput": ctx}
}

func readFailurePending(root string, event map[string]any, key string) (map[string]any, string, error) {
	path := filepath.Join(root, "pending", key+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		path = bypassPath(root, event)
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, path, errors.New("pending call record is missing or unreadable")
	}
	var pending map[string]any
	if json.Unmarshal(data, &pending) != nil || pending == nil {
		return nil, path, errors.New("pending call record is invalid")
	}
	return pending, path, nil
}

func telemetryDuration(value any) any {
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	parsed, err := number.Float64()
	if err != nil || parsed < 0 {
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
