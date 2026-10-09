package enforce

import (
	"os"
	"path/filepath"
	"strings"
)

// PostToolUseFailureAsyncFollowup mirrors Python's failed codex_result and
// codex_cancel follow-up path.
func PostToolUseFailureAsyncFollowup(event map[string]any, root string) map[string]any {
	tool := stringValue(event["tool_name"])
	if !codexTools[tool] || !(strings.HasSuffix(tool, "__codex_result") || strings.HasSuffix(tool, "__codex_cancel")) {
		return nil
	}
	if root == "" {
		return failureErrorOutput("state root is missing")
	}
	lock, index, pending, _, cached, err := claimAsyncResult(root, event, stringValue(event["cwd"]))
	if err != nil {
		diagnostic := postDiagnostic(event, err)
		diagnostic["phase"] = "PostToolUse"
		diagnostic["error"] = failureErrorText(err)
		_ = writeFailureDiagnostic(root, diagnostic)
		return asyncFailureInterrupted()
	}
	defer releaseTaskUsageLock(lock)
	if index["stage"] == "CONSUMED" || index["stage"] == "FAILED" {
		if response, ok := cached.(map[string]any); ok && response != nil {
			asyncFailureCleanupConsumed(root, index, pending, event)
			return response
		}
		return asyncFailureInterrupted()
	}
	if response, handled, finalizeErr := finalizeAsyncFailure(root, event, index, pending); handled {
		if finalizeErr == nil {
			return response
		}
		return failureErrorOutput(failureErrorText(finalizeErr))
	}

	failureEvent := make(map[string]any, len(event)+3)
	for key, value := range event {
		failureEvent[key] = value
	}
	failureEvent["hook_event_name"] = "PostToolUseFailure"
	failureEvent["tool_name"] = "mcp__codex-main__codex"
	failureEvent["session_id"], failureEvent["tool_use_id"] = index["session_id"], pending["tool_use_id"]
	if duration := asyncFailureDuration(asyncJob(root, index)); duration != nil {
		failureEvent["duration_ms"] = *duration
	} else {
		failureEvent["duration_ms"] = nil
	}
	response := PostToolUseFailure(failureEvent, root)
	if response == nil {
		response = failureErrorOutput("WiringError: shared failure finalizer returned no response")
	}
	indexPath := filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json")
	evidencePath, err := filepath.Abs(filepath.Join(root, "raw_tool_outputs", stringValue(index["call_key"])+"-failure.json"))
	if err != nil {
		return failureErrorOutput(failureErrorText(err))
	}
	if err := consumeAsyncResult(indexPath, index, evidencePath, response); err != nil {
		return failureErrorOutput(failureErrorText(err))
	}
	return response
}

func asyncFailureInterrupted() map[string]any {
	encoded, _ := canonicalJSON(map[string]any{"ambiguous_outcome": true,
		"claude_review_mode": "FULL", "status": "ALREADY_CONSUMED_OR_INTERRUPTED"})
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse",
		"updatedToolOutput": []any{map[string]any{"type": "text", "text": string(encoded)}}}}
}

func asyncFailureCleanupConsumed(root string, index, pending map[string]any, event map[string]any) {
	job := asyncJob(root, index)
	if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_cancel") && job != nil && job["discarded"] == true {
		return
	}
	if index["terminal_failure"] != true && index["stage"] != "FAILED" && job != nil && stringValue(job["worktreePath"]) != "" && job["applyState"] != "applied" {
		return
	}
	key := stringValue(index["call_key"])
	if asyncHash.MatchString(key) {
		project := stringValue(index["project_key"])
		if pending != nil {
			project = stringValue(pending["project_key"])
		}
		if project != "" {
			_ = leaseRelease(root, project, key)
		}
		_ = os.Remove(filepath.Join(root, "pending", key+".json"))
	}
}
