package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var codexTools = map[string]bool{
	"mcp__codex-main__codex": true, "mcp__codex-main__codex-reply": true,
	"mcp__plugin_purser_codex-main__codex": true, "mcp__plugin_purser_codex-main__codex-reply": true,
	"mcp__codex-main__codex_start": true, "mcp__codex-main__codex_result": true,
	"mcp__codex-main__codex_cancel": true, "mcp__plugin_purser_codex-main__codex_start": true,
	"mcp__plugin_purser_codex-main__codex_result": true, "mcp__plugin_purser_codex-main__codex_cancel": true,
}

// Pre dispatches Codex PreToolUse and recovers internal failures with Python's
// synchronous bypass / async-start deny contract. Normal policy remains Python-owned.
func Pre(event map[string]any, root string) (map[string]any, error) {
	if event["hook_event_name"] != "PreToolUse" || !codexTools[stringValue(event["tool_name"])] {
		return nil, nil
	}
	// Contract process tests require building the hook with -tags contracttest.
	internal := contractFault()
	if internal == nil {
		internal = errors.New("Go PreToolUse enforcement is not yet ported")
	}
	if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_start") {
		_, _ = os.Stderr.Write(append(diagnosticJSON(diagnostic(event, internal)), '\n'))
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "deny",
			"permissionDecisionReason": "ASYNC_ENFORCEMENT_FAILED: Async start lacked a durable route/baseline; FULL review is required.",
		}}, internal
	}
	return syncBypass(event, root, internal), internal
}

func syncBypass(event map[string]any, root string, cause error) map[string]any {
	reason := safeError(cause)
	recorded := false
	if root != "" {
		if err := writeBypass(root, event, reason); err == nil {
			recorded = true
		} else {
			reason += "; bypass persistence failed: " + safeError(err)
		}
	}
	if root != "" {
		if err := writeDiagnostic(root, event, cause); err != nil {
			_, _ = os.Stderr.Write(append(diagnosticJSON(diagnostic(event, cause)), '\n'))
		}
	} else {
		_, _ = os.Stderr.Write(append(diagnosticJSON(diagnostic(event, cause)), '\n'))
	}
	persistence := "The bypass record could not be persisted; this PreToolUse context is the only warning."
	if recorded {
		persistence = "The bypass was recorded for PostToolUse."
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "PreToolUse",
		"additionalContext": "Orchestrator routing was NOT enforced for this Codex call because of an internal error: " + reason + ". The call is allowed unmodified. " + persistence,
	}}
}

func writeBypass(root string, event map[string]any, reason string) error {
	path, err := pendingPath(root, event)
	if err != nil {
		path = bypassPath(root, event)
	}
	record := map[string]any{
		"kind": "pretool_bypass", "routing_enforced": false, "baseline_captured": false,
		"reason": reason, "task_id": nil, "project_key": "unresolved", "phase": nil,
		"route": nil, "model": nil, "effort": nil, "scope": []any{}, "repo_root": nil,
		"prior_thread_id": nil,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func pendingPath(root string, event map[string]any) (string, error) {
	id, ok := event["tool_use_id"].(string)
	if !ok || id == "" {
		return "", errors.New("tool_use_id is missing")
	}
	session, _ := event["session_id"].(string)
	sum := sha256.Sum256([]byte(session + "\x00" + id))
	return filepath.Join(root, "pending", hex.EncodeToString(sum[:])+".json"), nil
}

func bypassPath(root string, event map[string]any) string {
	input, _ := canonicalJSON(event["tool_input"])
	session, _ := event["session_id"].(string)
	identity := session + "\x00" + stringValue(event["tool_name"]) + "\x00" + string(input)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(root, "pending", "bypass-"+hex.EncodeToString(sum[:])+".json")
}

func writeDiagnostic(root string, event map[string]any, cause error) error {
	path := filepath.Join(root, "diagnostics.jsonl")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	data := append(diagnosticJSON(diagnostic(event, cause)), '\n')
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func diagnostic(event map[string]any, cause error) map[string]any {
	return map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "phase": "PreToolUse",
		"hook_event_name": event["hook_event_name"], "tool_name": event["tool_name"],
		"tool_use_id": event["tool_use_id"], "error": safeError(cause)}
}

func safeError(err error) string {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "RuntimeError"
	}
	return fmt.Sprintf("RuntimeError: %s", message)
}

func stringValue(value any) string { result, _ := value.(string); return result }

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }

func diagnosticJSON(value any) []byte {
	compact := mustJSON(value)
	formatted := make([]byte, 0, len(compact)+8)
	quoted, escaped := false, false
	for _, char := range compact {
		if quoted {
			formatted = append(formatted, char)
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				quoted = false
			}
			continue
		}
		formatted = append(formatted, char)
		if char == '"' {
			quoted = true
		} else if char == ':' || char == ',' {
			formatted = append(formatted, ' ')
		}
	}
	return formatted
}

func canonicalJSON(value any) ([]byte, error) {
	var output strings.Builder
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(output.String(), "\n")), nil
}
