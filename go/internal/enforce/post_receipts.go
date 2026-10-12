package enforce

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
)

func postReceiptStatePath(root, repository string) string {
	return filepath.Join(root, "projects", projectKey(&repository), "telemetry", "compaction_advisor.json")
}

func readPostReceiptState(path, session string) map[string]any {
	state := map[string]any{"session_id": session, "receipt_pending": nil, "dispatch_pending": nil,
		"maintenance_pending": nil, "version_pending": nil, "leader_pending": nil, "fallback_pending": nil}
	data, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	var saved map[string]any
	if json.Unmarshal(data, &saved) != nil || saved["session_id"] != session {
		return state
	}
	for key, value := range saved {
		state[key] = value
	}
	return state
}

func writePostReceiptState(path string, state map[string]any) {
	data, err := json.Marshal(state)
	if err == nil {
		_ = atomicWrite(path, data)
	}
}

// queuePostCompletionReceipt stores the finalized completion receipt for the session.
func queuePostCompletionReceipt(root string, event map[string]any, repository string, bundle map[string]any) map[string]any {
	session, model := stringValue(event["session_id"]), stringValue(bundle["model"])
	if repository == "" || session == "" || model == "" {
		return nil
	}
	effort := stringValue(bundle["effort"])
	if effort == "" {
		effort = "unknown"
	}
	receipt := map[string]any{"model": model, "effort": effort, "blocked": false}
	if up, err := contextio.Gear(defaultPreStateRoot(root), projectKey(&repository)); err == nil && up {
		receipt["gear"] = "up"
	}
	if quota, ok := bundle["codex_quota"].(map[string]any); ok {
		for _, key := range []string{"5h_used_pct", "5h_resets_at", "7d_used_pct", "7d_resets_at", "snapshot_at"} {
			value := quota[key]
			if strings.HasSuffix(key, "used_pct") {
				switch n := value.(type) {
				case float64:
					receipt[key] = math.Round(n)
				case int:
					receipt[key] = n
				}
			} else if _, ok := value.(string); ok {
				receipt[key] = value
			}
		}
	}
	path := postReceiptStatePath(root, repository)
	unlock, err := contextio.LockFile(path + ".lock")
	if err != nil {
		return nil
	}
	defer unlock()
	state := readPostReceiptState(path, session)
	state["receipt_pending"] = receipt
	writePostReceiptState(path, state)
	if stringValue(event["tool_use_id"]) == "" {
		return nil
	}
	advice := PostReceiptAdvice(receipt, time.Now().UTC())
	id, err := callKey(event)
	if err != nil {
		return nil
	}
	state["receipt_delivered"] = true
	writePostReceiptState(path, state)
	return map[string]any{"id": id, "message": advice}
}

// queuePostReceipt adds a visible dispatch receipt to the next-turn queue.
func queuePostReceipt(root string, event map[string]any, repository, line string) {
	session := stringValue(event["session_id"])
	if repository == "" || session == "" || line == "" {
		return
	}
	path := postReceiptStatePath(root, repository)
	state := readPostReceiptState(path, session)
	pending, _ := state["dispatch_pending"].(map[string]any)
	if pending == nil {
		pending = map[string]any{"lines": []any{}, "board": nil, "board_job_ids": []any{}, "blocked": false}
	}
	lines, _ := pending["lines"].([]any)
	for _, existing := range lines {
		if existing == line {
			return
		}
	}
	pending["lines"] = append(lines, line)
	pending["blocked"] = false
	state["dispatch_pending"] = pending
	writePostReceiptState(path, state)
}

// markPostGuidanceDelivered records that lifecycle guidance was emitted.
func markPostGuidanceDelivered(root string, event map[string]any, repository string) {
	session := stringValue(event["session_id"])
	if repository == "" || session == "" {
		return
	}
	path := postReceiptStatePath(root, repository)
	state := readPostReceiptState(path, session)
	changed := false
	for _, field := range []string{"maintenance_pending", "version_pending", "leader_pending", "fallback_pending"} {
		if pending, ok := state[field].(map[string]any); ok && pending["delivered"] != true {
			pending["delivered"] = true
			changed = true
		}
	}
	if changed {
		writePostReceiptState(path, state)
	}
}

// postPendingGuidance renders pending receipt and dispatch instructions.
func postPendingGuidance(root string, event map[string]any, repository string) string {
	session := stringValue(event["session_id"])
	if repository == "" || session == "" {
		return ""
	}
	state := readPostReceiptState(postReceiptStatePath(root, repository), session)
	parts := []string{}
	if receipt, ok := state["receipt_pending"].(map[string]any); ok && state["receipt_delivered"] != true {
		parts = append(parts, PostReceiptAdvice(receipt, time.Now().UTC()))
	}
	if pending, ok := state["dispatch_pending"].(map[string]any); ok {
		if lines, ok := pending["lines"].([]any); ok && len(lines) > 0 {
			valid, rendered := true, make([]string, 0, len(lines))
			for _, value := range lines {
				line, ok := value.(string)
				if !ok || line == "" {
					valid = false
					break
				}
				rendered = append(rendered, line)
			}
			if valid {
				parts = append(parts, "Write one line in the user's language as `> ⚓ **Codex: ...**`, including every exact dispatch receipt line below verbatim:\n"+strings.Join(rendered, "\n"))
			}
		}
	}
	return strings.Join(parts, "\n")
}

// Claim UI emission durably so cached lifecycle output never repeats a receipt.
func deliverPostCompletionReceipt(response map[string]any, root string) {
	receipt := mapValue(response["completion_receipt"])
	delete(response, "completion_receipt")
	id, message := stringValue(receipt["id"]), stringValue(receipt["message"])
	if !asyncHash.MatchString(id) || message == "" {
		return
	}
	path := filepath.Join(root, "receipt_deliveries", id+".json")
	unlock, err := contextio.LockFile(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if atomicAdmissionJSON(path, receipt) != nil {
		return
	}
	if prior := stringValue(response["systemMessage"]); prior != "" {
		message = prior + "\n" + message
	}
	response["systemMessage"] = message
}
