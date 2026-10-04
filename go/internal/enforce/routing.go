package enforce

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"purser-hook/internal/ledger"
	"purser-hook/internal/policy"
)

// ReplyRecovery is the synchronous codex-reply recovery decision before routing.
type ReplyRecovery struct {
	Code       string                 `json:"code"`
	Message    string                 `json:"message"`
	Root       *string                `json:"root"`
	Parsed     policy.OrchParseResult `json:"parsed"`
	Record     map[string]any         `json:"record"`
	LiveThread map[string]any         `json:"live_thread"`
	Label      *string                `json:"label"`
	Route      policy.RouteResolution `json:"route"`
}

// RecoverReply mirrors codex.py Pre's indexed-root, forbidden-input, bare-reply,
// mismatch, ledger, inherited-label and route checks, in that order.
func RecoverReply(event map[string]any, runtimeRoot string, explicitRoot *string, config map[string]any) ReplyRecovery {
	input, ok := event["tool_input"].(map[string]any)
	if !ok {
		return replyFailure("MALFORMED_TOOL_INPUT", "tool_input must be an object")
	}
	threadID := input["threadId"]
	indexedRoot, indexError := indexedReplyRoot(runtimeRoot, threadID)
	if indexError != "" {
		message := "The recorded project for this codex-reply thread is no longer a git work tree."
		if indexError == "INVALID_THREAD_ID" {
			message = "codex-reply threadId must be a simple identifier without path separators."
		}
		return replyFailure(indexError, message)
	}
	root, _ := resolveProjectRoot(event, explicitRoot)
	if indexedRoot != nil {
		root = indexedRoot
	}
	forbidden := make([]string, 0)
	for _, key := range []string{"model", "config", "developer-instructions"} {
		if _, exists := input[key]; exists {
			forbidden = append(forbidden, key)
		}
	}
	if len(forbidden) != 0 {
		sort.Strings(forbidden)
		return replyFailure("CODEX_REPLY_INPUT_INVALID", fmt.Sprintf("codex-reply accepts only threadId and prompt; field %s is not allowed on reply.", joinFields(forbidden)))
	}

	_, promptIsString := input["prompt"].(string)
	parsed := policy.ParseOrchPrompt(input["prompt"], config)
	replyWithoutOrch := parsed.Code == "MISSING_ORCH_LINE" && promptIsString
	var record map[string]any
	var live map[string]any
	if replyWithoutOrch {
		id, _ := threadID.(string)
		record = ledger.FindByThreadID(filepath.Join(runtimeRoot, "projects", projectKey(root)), id)
		if record == nil {
			return replyFailure("NO_LIVE_THREAD", "No recoverable ledger record exists for this codex-reply thread.")
		}
		live, _ = record["live_thread"].(map[string]any)
		if live == nil {
			return replyFailure("NO_LIVE_THREAD", "The codex-reply thread record has no recoverable live thread context.")
		}
		parsed, ok = inheritedReplyParse(input["prompt"], live)
		if !ok {
			return replyFailure("NO_LIVE_THREAD", "The codex-reply thread record lacks recoverable phase or scope context.")
		}
	} else {
		if !parsed.Valid {
			return replyFailure(parsed.Code, parsed.Message)
		}
		id, _ := threadID.(string)
		recorded := ledger.FindByThreadID(filepath.Join(runtimeRoot, "projects", projectKey(root)), id)
		if recorded != nil {
			recordedThread, _ := recorded["live_thread"].(map[string]any)
			recordedTask := recorded["task_id"]
			if recordedTask != *parsed.TaskID {
				return replyMismatch("THREAD_TASK_MISMATCH", "task", recordedTask, *parsed.TaskID, id)
			}
			if recordedThread != nil {
				for _, item := range []struct{ field, code string }{{"phase", "THREAD_PHASE_MISMATCH"}, {"route", "THREAD_ROUTE_MISMATCH"}} {
					if recordedThread[item.field] != *parsedValue(parsed, item.field) {
						return replyMismatch(item.code, item.field, recordedThread[item.field], *parsedValue(parsed, item.field), id)
					}
				}
			}
		}
		path, _ := ledger.PathForTask(filepath.Join(runtimeRoot, "projects", projectKey(root)), *parsed.TaskID)
		loaded := ledger.Load(path, *parsed.TaskID)
		if loaded.Status == ledger.CORRUPT {
			return replyFailure("INTERNAL_LEDGER_CORRUPT", "session ledger is unreadable")
		}
		if loaded.Status == ledger.OK {
			record = loaded.Record
			live, _ = record["live_thread"].(map[string]any)
		}
	}
	labelValue, _ := input["label"].(string)
	if labelValue == "" && live != nil {
		labelValue, _ = live["label"].(string)
	}
	labelValue = strings.TrimSpace(labelValue)
	if labelValue == "" {
		return replyFailure("MISSING_TASK_LABEL", "Fix: add `label` with a short human description in the user's language (1–80 characters); this thread has no saved label to reuse.")
	}
	if utf8.RuneCountInString(labelValue) > 80 {
		return replyFailure("MISSING_TASK_LABEL", "Fix: add `label` with a short human description in the user's language (1–80 characters); this thread has no saved label to reuse.")
	}
	label := labelValue
	route := policy.ResolveRoute(*parsed.Route, config)
	if !route.Valid {
		return ReplyRecovery{Code: route.Code, Message: route.Message, Root: root, Parsed: parsed, Record: record, LiveThread: live, Label: &label, Route: route}
	}
	return ReplyRecovery{Code: "VALID", Root: root, Parsed: parsed, Record: record, LiveThread: live, Label: &label, Route: route}
}

func inheritedReplyParse(prompt any, live map[string]any) (policy.OrchParseResult, bool) {
	task, taskOK := live["task_id"].(string)
	phase, phaseOK := live["phase"].(string)
	route, routeOK := live["route"].(string)
	scope, scopeOK := live["scope"].([]any)
	text, promptOK := prompt.(string)
	if !taskOK || !phaseOK || !routeOK || !scopeOK || !promptOK {
		return policy.OrchParseResult{}, false
	}
	paths := make([]string, 0, len(scope))
	for _, raw := range scope {
		path, ok := raw.(string)
		if !ok {
			return policy.OrchParseResult{}, false
		}
		paths = append(paths, path)
	}
	result := policy.OrchParseResult{Valid: true, Code: "VALID", Message: "ORCH metadata inherited from the recorded reply thread.", Prompt: text, TaskID: &task, Phase: &phase, Route: &route, Scope: paths}
	if reason, ok := live["reason"].(string); ok {
		result.Reason = &reason
	}
	return result, true
}

func replyFailure(code, message string) ReplyRecovery {
	return ReplyRecovery{Code: code, Message: message}
}
func replyMismatch(code, field string, old, requested any, threadID string) ReplyRecovery {
	caller := "codex-reply"
	message := fmt.Sprintf("thread %s was recorded for %s %v; this %s says %v. Reuse that %s or start a fresh thread.", threadID, field, old, caller, requested, field)
	if field == "task" {
		message = fmt.Sprintf("task id differs from thread %s's task %v; this %s requests %v. Reuse that task or start a fresh thread.", threadID, old, caller, requested)
	}
	return replyFailure(code, message)
}
func parsedValue(parsed policy.OrchParseResult, field string) *string {
	if field == "phase" {
		return parsed.Phase
	}
	return parsed.Route
}
func joinFields(fields []string) string {
	result := ""
	for i, field := range fields {
		if i > 0 {
			result += ", "
		}
		result += field
	}
	return result
}
