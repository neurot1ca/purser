package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/lease"
)

// decodeCodexResult mirrors codex.py's text/structuredContent result decoder.
func decodeCodexResult(response any) (string, string, error) {
	for _, object := range decodedCodexObjects(response) {
		thread, _ := object["threadId"].(string)
		if thread != "" {
			content, _ := object["content"].(string)
			return thread, content, nil
		}
	}
	return "", "", errors.New("thread id could not be recovered from Codex MCP text content")
}

func decodedCodexObjects(response any) []map[string]any {
	objects := make([]map[string]any, 0, 2)
	var payload any = response
	if envelope, ok := response.(map[string]any); ok {
		if structured, ok := envelope["structuredContent"].(map[string]any); ok {
			objects = append(objects, structured)
		}
		payload = envelope["content"]
	}
	var texts []string
	switch value := payload.(type) {
	case string:
		texts = append(texts, value)
	case []any:
		for _, block := range value {
			if item, ok := block.(map[string]any); ok && item["type"] == "text" {
				if text, ok := item["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
	}
	for _, text := range texts {
		var object map[string]any
		if json.Unmarshal([]byte(text), &object) == nil && object != nil {
			objects = append(objects, object)
		}
	}
	return objects
}

// baseBundle is the unreviewed FULL response skeleton from Python.
func baseBundle(internalError string) map[string]any {
	findings := []any{}
	if internalError != "" {
		findings = append(findings, map[string]any{"code": "INTERNAL_POST_ERROR", "message": internalError})
	}
	return map[string]any{
		"task_id": nil, "phase": nil, "implementer": "codex-main", "thread_id": nil,
		"route": nil, "model": nil, "effort": nil, "baseline_head": nil, "current_head": nil,
		"changed_paths": nil, "known_concurrent_scopes": []any{}, "whole_tree_delta": nil,
		"whole_tree_anomaly": nil, "changed_path_count": nil, "changed_paths_sha256": nil,
		"delta_lines": nil, "delta_files": nil, "validation": "UNKNOWN", "reviewer1_provider": nil,
		"reviewer1_result": nil, "reviewer1_skipped_reason": nil, "reviewer1_model_requested": nil,
		"served_model_attestation": "UNAVAILABLE", "routing_enforced": false, "findings": findings,
		"claude_review_mode": "FULL", "raw_output_path": nil, "full_bundle_path": nil,
		"protected_paths_touched": []any{}, "concurrent_writer": nil, "implementer_report": "",
		"audit_receipt": nil, "micro_repair": nil, "micro_repair_cleared_review": false,
		"task_contract_grounded": false, "worker_validation": nil, "hook_checks": nil,
		"project_root_reason": nil,
	}
}

func replacement(bundle map[string]any, field string) (map[string]any, error) {
	if field != "updatedToolOutput" && field != "updatedMCPToolOutput" {
		return nil, errors.New("post_tool_output_field is not a supported hook field")
	}
	encoded, err := canonicalJSON(bundle)
	if err != nil {
		return nil, err
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PostToolUse", field: []any{map[string]any{"type": "text", "text": string(encoded)}},
	}}, nil
}

func callKey(event map[string]any) (string, error) {
	id, ok := event["tool_use_id"].(string)
	if !ok || id == "" {
		return "", errors.New("tool_use_id is missing")
	}
	session, _ := event["session_id"].(string)
	sum := sha256.Sum256([]byte(session + "\x00" + id))
	return hex.EncodeToString(sum[:]), nil
}

func persistRawOutput(root string, event map[string]any, value any) (string, error) {
	key, err := callKey(event)
	if err != nil {
		return "", err
	}
	return persistRawOutputAs(root, key+".json", value)
}

func persistRawOutputAs(root, name string, value any) (string, error) {
	path := filepath.Join(root, "raw_tool_outputs", name)
	data, err := canonicalJSON(value)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve raw output path: %w", err)
	}
	return absolute, nil
}

// PostInternalError replaces an unreviewed raw result only when the Go Post
// handler has failed.
func PostInternalError(event map[string]any, root string) (map[string]any, error) {
	if event["hook_event_name"] != "PostToolUse" || !codexTools[stringValue(event["tool_name"])] {
		return nil, nil
	}
	cause := postFault()
	if cause == nil {
		return nil, nil
	}
	bundle := baseBundle(safeError(cause))
	var pending map[string]any
	var pendingFile string
	if root != "" {
		if rawPath, err := persistRawOutput(root, event, event["tool_response"]); err == nil {
			bundle["raw_output_path"] = rawPath
		}
		if key, err := callKey(event); err == nil {
			pendingFile = filepath.Join(root, "pending", key+".json")
			data, readErr := os.ReadFile(pendingFile)
			if readErr != nil {
				pendingFile = bypassPath(root, event)
				data, readErr = os.ReadFile(pendingFile)
			}
			if readErr == nil && json.Unmarshal(data, &pending) != nil {
				pending = nil
			}
		}
		if pending != nil {
			agent := stringValue(pending["agent"])
			if agent == "" {
				agent = "codex"
			}
			bundle["implementer"] = agent + "-main"
			for target, source := range map[string]string{
				"task_id": "task_id", "phase": "phase", "route": "route", "model": "model", "effort": "effort",
			} {
				bundle[target] = pending[source]
			}
			bundle["routing_enforced"] = pending["kind"] != "pretool_bypass"
			if pending["kind"] == "pretool_bypass" {
				bundle["findings"] = append(bundle["findings"].([]any), map[string]any{
					"code":    "PRETOOL_BYPASS_ROUTING_NOT_ENFORCED",
					"message": "Routing was NOT enforced for this call and no usable baseline was captured. Internal PreToolUse reason: " + stringValue(pending["reason"]),
				})
				if thread, report, decodeErr := decodeCodexResult(event["tool_response"]); decodeErr == nil {
					bundle["thread_id"], bundle["implementer_report"] = thread, report
				} else {
					bundle["findings"] = append(bundle["findings"].([]any), map[string]any{"code": "THREAD_ID_UNRECOVERABLE", "message": decodeErr.Error()})
				}
				_ = os.Remove(pendingFile)
			}
		}
		if err := writePostDiagnostic(root, event, cause); err != nil {
			_, _ = os.Stderr.Write(append(diagnosticJSON(postDiagnostic(event, cause)), '\n'))
		}
		if _, _, err := finalizeBundle(root, event, bundle, 8000, 1200); err != nil {
			// Keep the replacement fail-closed even when durable artifact writing fails.
			bundle["findings"] = append(bundle["findings"].([]any), map[string]any{"code": "INTERNAL_POST_ERROR", "message": safeError(err)})
		}
		if pending != nil && pending["kind"] != "pretool_bypass" && pending["async_call"] != true {
			cleanupErr := error(nil)
			if owner := stringValue(pending["lease_owner"]); owner != "" {
				project := stringValue(pending["project_key"])
				if project == "" {
					project = "unresolved"
				}
				result := lease.New(filepath.Join(root, "projects", project)).Release(owner, float64(time.Now().UnixNano())/1e9)
				if result.Status != "OK" {
					cleanupErr = fmt.Errorf("scope lease cleanup failed: %s", result.Code)
				}
			}
			if cleanupErr == nil {
				if err := os.Remove(pendingFile); err != nil && !errors.Is(err, os.ErrNotExist) {
					cleanupErr = fmt.Errorf("pending cleanup failed: %w", err)
				}
			}
			if cleanupErr != nil {
				_ = writePostDiagnostic(root, event, cleanupErr)
			}
		}
	}
	response, err := replacement(bundle, "updatedToolOutput")
	if err != nil {
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "updatedToolOutput": []any{map[string]any{"type": "text", "text": `{}`}}}}, cause
	}
	return response, cause
}

func postDiagnostic(event map[string]any, cause error) map[string]any {
	return map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "phase": "PostToolUse",
		"hook_event_name": event["hook_event_name"], "tool_name": event["tool_name"],
		"tool_use_id": event["tool_use_id"], "error": safeError(cause)}
}

func writePostDiagnostic(root string, event map[string]any, cause error) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, "diagnostics.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(diagnosticJSON(postDiagnostic(event, cause)), '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
