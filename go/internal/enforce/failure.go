package enforce

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
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
	evidence := map[string]any{
		"ambiguous_outcome": true, "work_performed": "UNKNOWN", "error": event["error"],
		"is_interrupt": event["is_interrupt"], "duration_ms": event["duration_ms"], "pending_call": pending,
	}
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
		var thread any
		if threadID := stringValue(pending["prior_thread_id"]); threadID != "" {
			thread = map[string]any{
				"task_id": pending["task_id"], "thread_id": threadID, "route": pending["route"],
				"model": pending["model"], "effort": pending["effort"], "task_contract_grounded": pending["task_contract_grounded"] == true,
			}
			threadMap := thread.(map[string]any)
			for _, field := range []string{"phase", "scope", "reason", "label", "task_objective", "acceptance_criteria"} {
				if value, ok := pending[field]; ok {
					threadMap[field] = value
				}
			}
			if threadMap["phase"] == "implementation" {
				if scope, ok := threadMap["scope"].([]any); !ok || len(scope) == 0 {
					delete(threadMap, "phase")
					delete(threadMap, "scope")
				}
			}
		}
		record := map[string]any{"task_id": pending["task_id"], "live_thread": thread,
			"pending_call_baseline": nil, "last_outcome": outcome}
		if saved := ledger.Save(filepath.Join(root, "projects", projectKey), record); saved.Status != ledger.OK {
			err = fmt.Errorf("ambiguous failure could not be recorded: %s", saved.Code)
		}
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

func persistFailureEvidence(root, key string, evidence map[string]any) (string, error) {
	return persistRawOutputAs(root, key+"-failure.json", evidence)
}

// Failure telemetry is best effort: inability to observe must not affect cleanup.
func failureObservation(root string, event map[string]any, pending map[string]any) {
	defer func() { _ = recover() }()
	taskID := ""
	if raw := stringValue(pending["task_id"]); raw != "" {
		taskID = safeEventID(raw)
	}
	projectKey := stringValue(pending["project_key"])
	if projectKey == "" {
		projectKey = "unresolved"
	}
	code := "CODEX_CALL_FAILED"
	if event["is_interrupt"] == true {
		code = "CODEX_CALL_INTERRUPTED"
	}
	payload := map[string]any{"failure_code": code, "duration_ms": telemetryDuration(event["duration_ms"]),
		"eta_minutes": pending["eta_minutes"], "reason": pending["reason"], "reason_source": pending["reason_source"],
		"previous_route": pending["previous_route"], "new_route": pending["new_route"]}
	for name, value := range payload {
		if value == nil && name != "eta_minutes" {
			delete(payload, name)
		}
	}
	now := time.Now().UTC()
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return
	}
	uuid[6] = uuid[6]&0x0f | 0x40
	uuid[8] = uuid[8]&0x3f | 0x80
	eventID := hex.EncodeToString(uuid[:])
	eventID = eventID[:8] + "-" + eventID[8:12] + "-" + eventID[12:16] + "-" + eventID[16:20] + "-" + eventID[20:]
	sessionID := safeEventID(stringValue(event["session_id"]))
	var sessionValue any = sessionID
	if sessionID == "unknown" {
		sessionValue = nil
	}
	observation := map[string]any{"schema_version": 1, "event_id": eventID, "ts_utc": now.Format(time.RFC3339Nano),
		"event_type": "IMPLEMENTER_FAILED", "task_id": nil, "session_id": sessionValue, "project_key": projectKey,
		"repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil, "payload": payload}
	if taskID != "" {
		observation["task_id"] = taskID
	}
	encoded, err := canonicalJSON(observation)
	if err != nil {
		return
	}
	if err = os.MkdirAll(filepath.Join(root, "events"), 0700); err != nil {
		return
	}
	release, err := lease.Lock(filepath.Join(root, "events", ".append.lock"))
	if err != nil {
		return
	}
	defer release()
	line := append(encoded, '\n')
	month := now.Format("2006-01")
	paths := []string{filepath.Join(root, "events", month, "session_"+sessionID+".jsonl")}
	if taskID != "" {
		paths = append(paths, filepath.Join(root, "events", "by_task", taskID+".jsonl"))
	}
	for _, path := range paths {
		if os.MkdirAll(filepath.Dir(path), 0700) != nil {
			return
		}
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			return
		}
		_, writeErr := f.Write(line)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return
		}
	}
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
