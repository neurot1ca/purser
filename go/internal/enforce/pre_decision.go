package enforce

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/ledger"
	"purser-hook/internal/policy"
	"purser-hook/internal/profile"
)

// preDecision assembles the existing Pre policy pieces. Success persistence is
// intentionally left to pass 64.
func preDecision(event map[string]any, runtimeRoot string) (map[string]any, error) {
	config, err := loadPreConfig()
	if err != nil {
		return nil, err
	}
	input, ok := event["tool_input"].(map[string]any)
	if !ok {
		return denyPre("MALFORMED_TOOL_INPUT", "tool_input must be an object"), nil
	}
	reply := strings.HasSuffix(stringValue(event["tool_name"]), "__codex-reply")
	parsed := policy.ParseOrchPrompt(input["prompt"], config)
	bareReply := reply && parsed.Code == "MISSING_ORCH_LINE" && stringValue(input["prompt"]) != ""
	if !parsed.Valid && !bareReply {
		if parsed.Code == "CONFIG_INVALID" {
			return nil, errors.New(parsed.Message)
		}
		return denyPre(parsed.Code, parsed.Message), nil
	}

	var root *string
	var live map[string]any
	if reply {
		recovery := RecoverReply(event, runtimeRoot, nil, config)
		if recovery.Code != "VALID" {
			return denyPre(recovery.Code, recovery.Message), nil
		}
		parsed, root, live = recovery.Parsed, recovery.Root, recovery.LiveThread
	} else {
		root, _ = resolveProjectRoot(event, nil)
	}
	if !parsed.Valid {
		return denyPre(parsed.Code, parsed.Message), nil
	}
	if root != nil && parsed.Phase != nil && *parsed.Phase != "operation" &&
		ClaudeRunsFullSuiteAtAcceptance(*root) && AsksWorkerToRunFullSuite(stringValue(input["prompt"])) {
		return denyPre("FULL_SUITE_REQUEST", "Ask the worker for focused tests only."), nil
	}
	route := policy.ResolveRoute(*parsed.Route, config)
	if !route.Valid {
		if route.Code == "CONFIG_INVALID" {
			return nil, errors.New(route.Message)
		}
		return denyPre(route.Code, route.Message), nil
	}
	requested := map[string]any{"task_id": *parsed.TaskID, "route": *parsed.Route, "model": *route.Model, "effort": *route.Effort}
	if reply {
		compat := policy.ThreadCompatibility(requested, live)
		if !compat.Compatible {
			return denyPre(compat.Code, compat.Message), nil
		}
	} else if root != nil {
		path, _ := ledger.PathForTask(filepath.Join(runtimeRoot, "projects", projectKey(root)), *parsed.TaskID)
		loaded := ledger.Load(path, *parsed.TaskID)
		if loaded.Status == ledger.CORRUPT {
			return nil, errors.New("session ledger is unreadable")
		}
		if loaded.Status == ledger.OK {
			live, _ = loaded.Record["live_thread"].(map[string]any)
		}
		fresh := policy.DecideFreshCodex(requested, live, parsed.Reason)
		if !fresh.Allowed {
			return denyPre(fresh.Code, fresh.Message), nil
		}
	}

	var active map[string]any
	if root != nil {
		active, err = contextio.ReadProjectActiveCoder(*root, runtimeRoot)
		if err != nil {
			return nil, err
		}
	}
	quotaHome := os.Getenv("CODEX_HOME")
	if quotaHome == "" {
		home, _ := os.UserHomeDir()
		quotaHome = filepath.Join(home, ".codex")
	}
	quota, _ := contextio.LatestQuota(quotaHome, time.Now().UTC())
	selection := SelectCoder(*parsed.Route, *route.Model, *route.Effort, reason(parsed), !reply, reply, config, input, live, active, preOpenCodeCandidates(runtimeRoot))
	if selection.Code == "THREAD_AGENT_MISMATCH" {
		return denyPre(selection.Code, "thread belongs to a different coding agent; start a fresh thread."), nil
	}
	if !reply && stringValue(config["coder_agent"]) == "codex" {
		fallback := mapValue(config["fallback"])
		decision := decideFallbackPre(parsed, stringValue(event["session_id"]), active, fallback, quota, nil, time.Now())
		switch decision.Action {
		case "deny":
			return denyPre(decision.Code, strings.TrimPrefix(decision.Message, decision.Code+": ")), nil
		case "approve", "decline":
			reset := any(nil)
			if quota != nil {
				reset = quota["5h_resets_at"]
			}
			changed, applyErr := contextio.ApplyFallbackConsent(*root, runtimeRoot, stringValue(event["session_id"]), decision.Action == "approve", reset, float64(time.Now().UnixNano())/1e9)
			if applyErr != nil {
				return nil, applyErr
			}
			if !changed {
				return denyPre("FALLBACK_CONSENT_REQUIRED", "The fallback approval is no longer current."), nil
			}
			active, err = contextio.ReadProjectActiveCoder(*root, runtimeRoot)
			if err != nil {
				return nil, err
			}
			selection = SelectCoder(*parsed.Route, *route.Model, *route.Effort, reason(parsed), !reply, reply, config, input, live, active, nil)
		case "question":
			return nil, errors.New("fallback question recording and delivery are not yet wired")
		}
		if root != nil && fallback != nil && active != nil && active["active_coder"] == "fallback" && parsed.Phase != nil && *parsed.Phase == "implementation" {
			action := decideFallbackReturn(active, stringValue(event["session_id"]), reason(parsed), quota, time.Now())
			evidence, _ := active["triggering_evidence"].(map[string]any)
			kind, triggerID := stringValue(evidence["type"]), evidence["trigger_id"]
			switch action {
			case "deny":
				return denyPre("PRIMARY_RETURN_CONSENT_REQUIRED", "Wait for the return question to be shown and answered before dispatch."), nil
			case "offer":
				return nil, errors.New("quota-renewal return question recording and delivery are not yet wired")
			case "accept", "decline":
				changed, applyErr := contextio.ApplyPrimaryReturnConsent(*root, runtimeRoot, stringValue(event["session_id"]), kind, triggerID, action == "accept")
				if applyErr != nil {
					return nil, applyErr
				}
				if !changed {
					return denyPre("PRIMARY_RETURN_CONSENT_REQUIRED", "The fallback state changed before return approval could be applied."), nil
				}
				active, err = contextio.ReadProjectActiveCoder(*root, runtimeRoot)
				if err != nil {
					return nil, err
				}
				selection = SelectCoder(*parsed.Route, *route.Model, *route.Effort, reason(parsed), !reply, reply, config, input, live, active, nil)
			case "claim_probe":
				_, probeErr := ProbeFallback(*root, runtimeRoot, kind, triggerID, event, []string{"codex"}, *route.Model)
				if probeErr != nil {
					return nil, probeErr
				}
			case "cooldown_return":
				_, updateErr := contextio.UpdateFallbackTrigger(*root, runtimeRoot, kind, triggerID, func(state map[string]any) {
					state["active_coder"], state["triggering_evidence"], state["reset_cycle_marker"], state["last_probe_time"] = "primary", nil, nil, nil
				})
				if updateErr != nil {
					return nil, updateErr
				}
			}
		}
	}

	prof := profile.ResolvedProfile{FullSuiteRequired: false}
	if root != nil {
		prof = profile.LoadProfile(*root)
	}
	rewrite := make(map[string]any, len(input))
	for key, value := range input {
		rewrite[key] = value
	}
	rewrite["prompt"] = parsed.Prompt
	updated := RewriteInstructionInput(rewrite, pointerValue(parsed.Phase), !reply, selection.Agent, prof.FullSuiteRequired)
	delete(updated, "label")
	if selection.Agent != stringValue(config["coder_agent"]) && selection.Agent != "" {
		updated["agent"] = selection.Agent
	}
	if root != nil && (pointerValue(parsed.Phase) == "implementation" || pointerValue(parsed.Phase) == "plan") {
		path, status := refreshCodeMapForPre(*root)
		if status != "FRESH" && status != "STALE" {
			path = filepath.Join(defaultPreStateRoot(runtimeRoot), "projects", projectKey(root), "code-map", "map.txt")
			status = "UNAVAILABLE"
		}
		updated["prompt"] = appendPreCodeMap(stringValue(updated["prompt"]), path, status)
	} else if root == nil && (pointerValue(parsed.Phase) == "implementation" || pointerValue(parsed.Phase) == "plan") {
		updated["prompt"] = appendPreCodeMap(stringValue(updated["prompt"]), filepath.Join(defaultPreStateRoot(runtimeRoot), "projects", "unresolved", "code-map", "map.txt"), "UNAVAILABLE")
	}
	if !reply {
		updated["model"] = selection.Model
		codexConfig, ok := input["config"].(map[string]any)
		if _, supplied := input["config"]; supplied && !ok {
			return nil, errors.New("tool_input.config must be an object when supplied")
		}
		if !ok {
			codexConfig = map[string]any{}
		}
		updatedConfig := map[string]any{}
		for key, value := range codexConfig {
			updatedConfig[key] = value
		}
		if selection.Effort != "none" && selection.Agent != "opencode" {
			updatedConfig["model_reasoning_effort"] = selection.Effort
		}
		if outputLimit := config["tool_output_token_limit"]; selection.Agent == "codex" && outputLimit != nil {
			updatedConfig["tool_output_token_limit"] = outputLimit
		}
		if pointerValue(parsed.Phase) == "plan" && selection.Agent == "codex" {
			updated["sandbox"], updated["approval-policy"] = "read-only", "never"
		}
		updated["config"] = updatedConfig
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "updatedInput": updated},
		"systemMessage": fmt.Sprintf("Purser: route=%s -> %s / %s", *parsed.Route, selection.Model, selection.Effort)}, nil
}

func preOpenCodeCandidates(runtimeRoot string) []any {
	if runtimeRoot == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(runtimeRoot, "opencode", "candidates.json"))
	if err != nil {
		return nil
	}
	var value struct {
		Candidates []any `json:"candidates"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil
	}
	return value.Candidates
}

func loadPreConfig() (map[string]any, error) {
	pluginRoot := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if pluginRoot == "" {
		cwd, _ := os.Getwd()
		for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, ".claude", "hooks", "orchestrator", "config.json")); err == nil {
				pluginRoot = dir
				break
			}
		}
	}
	if pluginRoot == "" {
		return nil, errors.New("orchestrator config path is unavailable")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return policy.LoadCoderConfig(filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json"), filepath.Join(home, ".orchestrator", "config.toml"))
}

func denyPre(code, message string) map[string]any {
	reason := code + ": " + message
	if strings.Contains(" MISSING_ORCH_LINE MALFORMED_ORCH_LINE UNKNOWN_KEY MISSING_REQUIRED_KEY INVALID_ENUM_VALUE SOL_ROUTE_NEEDS_BASIS UNBOUNDED_SCOPE NO_LIVE_THREAD ", " "+code+" ") {
		reason += "\n\n" + `Prepend this as the FIRST line of the prompt; the hook removes it before Codex sees it:
  ORCH|task=<stable-id>|phase=<implementation|operation>|route=<route>
  Add |scope=<path[,path...]> for implementation; omit it for operation.
  route  mechanical -> luna/high | subtle -> luna/high | normal -> sol/medium | consequential -> sol/high | escalated -> sol/xhigh | operation -> luna/high
  reason is optional; basis is required for normal, consequential, and escalated, and optional otherwise. eta=<N>m is optional; N must be an integer from 1..600. Reason, when supplied, is one of new_task | continue | fix | route_change | reanchor | repeated_failure | user_override.
  phase  'operation' for deploy, runner control, status, or read-only investigation; it skips Reviewer-1. 'implementation' for anything that changes code.
  scope  required for implementation; the paths the delegation may touch. It bounds the baseline scan, so it cannot be the whole repository.
  codex-reply may omit ORCH only when its threadId identifies a recorded thread; otherwise the reply is denied.
See .claude/skills/codex-orchestrator/SKILL.md for how to choose the route.`
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}}
}

func mapValue(value any) map[string]any { result, _ := value.(map[string]any); return result }
func defaultPreStateRoot(root string) string {
	if root != "" {
		return root
	}
	if value := os.Getenv("PURSER_STATE_DIR"); value != "" {
		return value
	}
	if value := os.Getenv("CLAUDE_CONFIG_DIR"); value != "" {
		return filepath.Join(value, "purser")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "purser")
}
func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func appendPreCodeMap(prompt, path, status string) string {
	if strings.Contains(prompt, "CODE_MAP=") {
		return prompt
	}
	return strings.TrimRight(prompt, " \t\r\n") + "\nCODE_MAP=" + path + " STATUS=" + status
}
