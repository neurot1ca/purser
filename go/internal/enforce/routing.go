package enforce

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"purser-hook/internal/ledger"
	"purser-hook/internal/models"
	"purser-hook/internal/policy"
)

// CoderSelection mirrors the coder/model/effort choice made by codex.Pre.
type CoderSelection struct {
	Agent       string `json:"agent"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	ModelSource string `json:"model_source"`
	Reason      string `json:"reason"`
	Code        string `json:"code"`
}

// SelectCoder keeps reply identity ahead of new-call choices, then applies
// mechanical candidates, active fallback, and explicit Codex overrides in Pre order.
func SelectCoder(route, routeModel, routeEffort, parsedReason string, newCall, replyCall bool,
	config, toolInput, liveThread, active map[string]any, candidates []any, gearUp bool) CoderSelection {
	agent, _ := config["coder_agent"].(string)
	if agent == "" {
		agent = "codex"
	}
	selection := CoderSelection{Agent: agent, Model: routeModel, Effort: routeEffort, ModelSource: "route", Reason: parsedReason, Code: "VALID"}
	fallback, _ := config["fallback"].(map[string]any)
	fallbackActive := newCall && fallback != nil && active != nil && active["active_coder"] == "fallback"
	mechanicalCandidate := false
	explicitModel, _ := toolInput["model"].(string)
	hasExplicitModel := codexModelPattern.MatchString(explicitModel)

	if replyCall && liveThread != nil {
		threadAgent, ok := liveThread["agent"].(string)
		if !ok || threadAgent == "" {
			threadAgent = "codex"
		}
		allowed := threadAgent == agent
		if fallback != nil && threadAgent == fallback["agent"] {
			allowed = true
		}
		mechanicalThread := threadAgent == "opencode" && liveThread["route"] == "mechanical" &&
			liveThread["phase"] == "implementation" && config["mechanical_opencode_enabled"] != false
		if !allowed && !mechanicalThread {
			selection.Code = "THREAD_AGENT_MISMATCH"
			return selection
		}
		selection.Agent = threadAgent
		if model, ok := liveThread["model"].(string); ok {
			selection.Model = model
		}
		if effort, ok := liveThread["effort"].(string); ok {
			selection.Effort = effort
		}
	} else if newCall && !replyCall && route == "mechanical" && config["mechanical_opencode_enabled"] != false && !hasExplicitModel {
		if model := firstOpenCodeCandidate(candidates); model != "" {
			selection.Agent, selection.Model = "opencode", model
			mechanicalCandidate = true
		}
	}
	if fallbackActive {
		selection.Agent, _ = fallback["agent"].(string)
		selection.Model, _ = fallback["model"].(string)
		if effort, ok := fallback["effort"].(string); ok {
			selection.Effort = effort
		} else {
			switch selection.Agent {
			case "agy":
				selection.Effort = "medium"
			case "opencode":
				selection.Effort = "none"
			}
		}
		selection.ModelSource = "route"
		mechanicalCandidate = false
	} else if newCall && !replyCall && !mechanicalCandidate && selection.Agent == agent {
		if hasExplicitModel {
			selection.Model = explicitModel
			selection.ModelSource = "override"
			if values, ok := toolInput["config"].(map[string]any); ok {
				if effort, ok := values["model_reasoning_effort"].(string); ok &&
					(effort == "low" || effort == "medium" || effort == "high" || effort == "xhigh") {
					selection.Effort = effort
				}
			}
		}
	}
	if gearUp && newCall && !replyCall && !fallbackActive && selection.Agent == agent && agent == "codex" {
		routes, _ := config["routes"].(map[string]any)
		entry, _ := routes[route].(map[string]any)
		model, _ := entry["model"].(string)
		effort, _ := entry["effort"].(string)
		if targetModel, targetEffort, changed := policy.GearTarget(config, model, effort); changed {
			selection.Model, selection.Effort = targetModel, targetEffort
			selection.ModelSource = "route"
		}
	}
	if replyCall && liveThread != nil && parsedReason == "" {
		if reason, ok := liveThread["reason"].(string); ok {
			selection.Reason = reason
		}
	}
	if newCall && !replyCall {
		selection.Model = models.ResolveLatest(selection.Agent, selection.Model)
	}
	return selection
}

var codexModelPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func firstOpenCodeCandidate(candidates []any) string {
	measured := make([]string, 0)
	seeded := make([]string, 0)
	for _, raw := range candidates {
		item, ok := raw.(map[string]any)
		if !ok || item["status"] == "failed" {
			continue
		}
		id, ok := item["id"].(string)
		if !ok || !opencodeCandidatePattern.MatchString(id) {
			continue
		}
		if item["status"] == "measured" {
			measured = append(measured, id)
			continue
		}
		benchmarks, _ := item["benchmarks"].(map[string]any)
		result, _ := benchmarks["1"].(map[string]any)
		passed, passedOK := exactInt(result["passed_count"])
		total, totalOK := exactInt(result["total"])
		if passedOK && totalOK && total == 6 && passed >= 5 && passed <= total {
			seeded = append(seeded, id)
		}
	}
	if len(measured) != 0 {
		return measured[0]
	}
	if len(seeded) != 0 {
		return seeded[0]
	}
	return ""
}

var opencodeCandidatePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

func exactInt(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), true
	case json.Number:
		integer, err := number.Int64()
		return int(integer), err == nil
	}
	return 0, false
}

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
	route := policy.ResolveRoute(*parsed.Route, config, false)
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
