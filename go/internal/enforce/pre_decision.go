package enforce

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
	"purser-hook/internal/models"
	"purser-hook/internal/pluginroot"
	"purser-hook/internal/policy"
	"purser-hook/internal/profile"
)

var afterPreMetadata = func() {}
var planMilestoneTaskID = regexp.MustCompile(`^(.+)-m([0-9]+)of([0-9]+)$`)

// preDecision assembles the existing Pre policy pieces. Success persistence is
// intentionally left to pass 64.
func preDecision(event map[string]any, runtimeRoot string, bypassMetadata map[string]any) (map[string]any, error) {
	config, err := loadPreConfig()
	if err != nil {
		return nil, err
	}
	input, ok := event["tool_input"].(map[string]any)
	if !ok {
		return denyPre("MALFORMED_TOOL_INPUT", "tool_input must be an object"), nil
	}
	tool := stringValue(event["tool_name"])
	isStart := strings.HasSuffix(tool, "__codex_start")
	reply := strings.HasSuffix(tool, "__codex-reply")
	mode := "new"
	if isStart {
		mode = stringValue(input["mode"])
		if mode == "" {
			mode = "new"
		}
		reply = mode == "reply"
		if mode != "new" && mode != "reply" {
			return denyPre("INVALID_ASYNC_MODE", "codex_start mode must be new or reply."), nil
		}
	}
	parsed := policy.ParseOrchPrompt(input["prompt"], config)
	bareReply := reply && parsed.Code == "MISSING_ORCH_LINE" && stringValue(input["prompt"]) != ""
	if !parsed.Valid && !bareReply {
		if parsed.Code == "CONFIG_INVALID" {
			return nil, errors.New(parsed.Message)
		}
		return denyPre(parsed.Code, parsed.Message), nil
	}

	var root *string
	rootReason := ""
	var live map[string]any
	if reply {
		recovery := RecoverReply(event, runtimeRoot, nil, config)
		if recovery.Code != "VALID" {
			return denyPre(recovery.Code, recovery.Message), nil
		}
		parsed, root, live = recovery.Parsed, recovery.Root, recovery.LiveThread
	} else {
		root, rootReason = resolveProjectRoot(event, nil)
	}
	if !parsed.Valid {
		return denyPre(parsed.Code, parsed.Message), nil
	}
	label := inputLabel(event)
	if (label == nil || label == "") && reply {
		label = live["label"]
	}
	label = planMilestoneLabel(pointerValue(parsed.TaskID), label)
	if root != nil && parsed.Phase != nil && *parsed.Phase != "operation" &&
		ClaudeRunsFullSuiteAtAcceptance(*root) {
		if phrase := WorkerFullSuiteRequestPhrase(stringValue(input["prompt"])); phrase != "" {
			return denyPre("FULL_SUITE_REQUEST", "Ask the worker for focused tests only. Matched phrase: \""+phrase+"\"."), nil
		}
	}
	gearUp := false
	if root != nil && !reply && config["coder_agent"] == "codex" {
		gearUp, err = contextio.Gear(defaultPreStateRoot(runtimeRoot), projectKey(root))
		if err != nil {
			return nil, fmt.Errorf("gear state unreadable: %w", err)
		}
	}
	route := policy.ResolveRoute(*parsed.Route, config, gearUp)
	if !route.Valid {
		if route.Code == "CONFIG_INVALID" {
			return nil, errors.New(route.Message)
		}
		return denyPre(route.Code, route.Message), nil
	}
	if !reply && pointerValue(parsed.Phase) == "implementation" && (parsed.Reason == nil || *parsed.Reason == "new_task") &&
		*parsed.Route != "mechanical" && *parsed.Route != "operation" &&
		!hasCompletedPlan(defaultPreStateRoot(runtimeRoot), pointerValue(parsed.TaskID)) {
		nonTestCount := nonTestScopeCount(parsed.Scope)
		why := ""
		if nonTestCount >= 5 {
			why = fmt.Sprintf("%d non-test scope paths", nonTestCount)
		} else if *parsed.Route == "consequential" || *parsed.Route == "escalated" {
			why = "route=" + *parsed.Route
		}
		if why != "" {
			project := projectKey(root)
			preEmit(runtimeRoot, event, root, pointerValue(parsed.TaskID), project, "PLAN_REDIRECT", map[string]any{
				"route": *parsed.Route, "non_test_scope_count": nonTestCount,
			})
			message := fmt.Sprintf("Purser: this is a large new task (%s). Run /purser:plan with the same goal and scope first, then implement its milestones as <plan-task>-m<N>of<T>. If the user explicitly asked to skip planning, resend with reason=user_override.", why)
			return map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": message,
			}}, nil
		}
	}
	if !reply {
		*route.Model = models.ResolveLatest(stringValue(config["coder_agent"]), *route.Model)
	} else if liveModel := stringValue(live["model"]); models.SameFamily(*route.Model, liveModel) {
		*route.Model = liveModel
	}
	bypassMetadata["task_id"], bypassMetadata["phase"], bypassMetadata["route"] = *parsed.TaskID, *parsed.Phase, *parsed.Route
	bypassMetadata["model"], bypassMetadata["effort"], bypassMetadata["scope"] = *route.Model, *route.Effort, stringSliceAny(parsed.Scope)
	var bypassETA any
	if parsed.ETAMinutes != nil {
		bypassETA = *parsed.ETAMinutes
	}
	bypassMetadata["reason"], bypassMetadata["eta_minutes"] = optionalString(parsed.Reason), bypassETA
	bypassMetadata["repo_root"], bypassMetadata["project_key"] = root, projectKey(root)
	if rootReason != "" {
		bypassMetadata["project_root_reason"] = rootReason
	} else {
		bypassMetadata["project_root_reason"] = nil
	}
	if reply && live != nil {
		bypassMetadata["prior_thread_id"] = live["thread_id"]
	}
	afterPreMetadata()
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
			if isStart && !reply && loaded.Record["last_outcome"] != nil {
				return denyPre("TASK_ALREADY_FINISHED", "This task already finished; use a fresh task id."), nil
			}
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
	selection := SelectCoder(*parsed.Route, *route.Model, *route.Effort, reason(parsed), !reply, reply, config, input, live, active, preOpenCodeCandidates(runtimeRoot), gearUp)
	if selection.Code == "THREAD_AGENT_MISMATCH" {
		return denyPre(selection.Code, "thread belongs to a different coding agent; start a fresh thread."), nil
	}
	if !reply && stringValue(config["coder_agent"]) == "codex" {
		fallback := mapValue(config["fallback"])
		var launchFailure map[string]any
		if root != nil {
			launchFailure = contextio.LastLaunchFailure(defaultPreStateRoot(runtimeRoot), projectKey(root))
		}
		decision := decideFallbackPre(parsed, stringValue(event["session_id"]), active, fallback, quota, launchFailure, time.Now())
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
			selection = SelectCoder(*parsed.Route, *route.Model, *route.Effort, reason(parsed), !reply, reply, config, input, live, active, nil, gearUp)
		case "question":
			evidence := make(map[string]any, len(decision.Trigger)+2)
			for key, value := range decision.Trigger {
				evidence[key] = value
			}
			evidence["kind"], evidence["question"] = decision.Kind, decision.Question
			recorded, recordErr := contextio.RecordFallbackQuestion(*root, runtimeRoot, evidence, stringValue(event["session_id"]))
			if recordErr != nil {
				return nil, recordErr
			}
			if recorded {
				contextio.QueueFallbackQuestion(event, *root, decision.Question, stringValue(fallback["model"]), false, runtimeRoot)
			}
			return denyPre(decision.Code, decision.Message), nil
		}
		if root != nil && fallback != nil && active != nil && active["active_coder"] == "fallback" && parsed.Phase != nil && *parsed.Phase == "implementation" {
			action := decideFallbackReturn(active, stringValue(event["session_id"]), reason(parsed), quota, time.Now())
			evidence, _ := active["triggering_evidence"].(map[string]any)
			kind, triggerID := stringValue(evidence["type"]), evidence["trigger_id"]
			switch action {
			case "deny":
				return denyPre("PRIMARY_RETURN_CONSENT_REQUIRED", "Wait for the return question to be shown and answered before dispatch."), nil
			case "offer":
				primaryRoute := policy.ResolveRoute(*parsed.Route, config, false)
				if !primaryRoute.Valid {
					return denyPre("PRIMARY_RETURN_ROUTE_INVALID", primaryRoute.Message), nil
				}
				line := fmt.Sprintf("> ⚓ **%s's quota has renewed. Return to the primary coder?**", *primaryRoute.Model)
				recorded, recordErr := contextio.RecordPrimaryReturnQuestion(*root, runtimeRoot, kind, triggerID,
					stringValue(event["session_id"]), line, quota["5h_resets_at"])
				if recordErr != nil {
					return nil, recordErr
				}
				if recorded {
					contextio.QueueFallbackQuestion(event, *root, line, *primaryRoute.Model, false, runtimeRoot)
				}
				return denyPre("PRIMARY_RETURN_CONSENT_REQUIRED", "A return-to-primary question must be shown before another implementation dispatch."), nil
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
				selection = SelectCoder(*parsed.Route, *route.Model, *route.Effort, reason(parsed), !reply, reply, config, input, live, active, nil, gearUp)
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
		path, status := refreshCodeMapForPre(*root, defaultPreStateRoot(runtimeRoot))
		if status != "FRESH" && status != "STALE" {
			path = filepath.Join(defaultPreStateRoot(runtimeRoot), "projects", projectKey(root), "code-map", "map.txt")
			status = "UNAVAILABLE"
		}
		updated["prompt"] = appendPreCodeMap(stringValue(updated["prompt"]), path, status, parsed.Scope)
	} else if root == nil && (pointerValue(parsed.Phase) == "implementation" || pointerValue(parsed.Phase) == "plan") {
		updated["prompt"] = appendPreCodeMap(stringValue(updated["prompt"]), filepath.Join(defaultPreStateRoot(runtimeRoot), "projects", "unresolved", "code-map", "map.txt"), "UNAVAILABLE")
	}
	updated["prompt"] = appendPrePlanShape(stringValue(updated["prompt"]), pointerValue(parsed.Phase))
	if !reply {
		updated["model"] = selection.Model
		if fallback := mapValue(config["fallback"]); fallback != nil && active != nil && active["active_coder"] == "fallback" && selection.Agent == stringValue(fallback["agent"]) {
			updated["agent"] = selection.Agent
		}
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
		if pointerValue(parsed.Phase) == "plan" && (selection.Agent == "codex" || selection.Agent == "claude") {
			updated["sandbox"], updated["approval-policy"] = "read-only", "never"
		}
		updated["config"] = updatedConfig
	}
	callID := ""
	inputHash := ""
	var pendingETA *int
	worktreeStart := isStart && mode == "new" && pointerValue(parsed.Phase) == "implementation"
	var worktreeReplyPath *string
	if isStart && mode == "reply" && pointerValue(parsed.Phase) == "implementation" {
		if path := preThreadWorktreePath(runtimeRoot, input["threadId"]); path != "" {
			worktreeReplyPath = &path
		}
	}
	worktreeReply := worktreeReplyPath != nil
	if isStart {
		pendingETA = parsed.ETAMinutes
		if supplied, ok := intValue(input["etaMinutes"]); ok {
			pendingETA = &supplied
		}
		if pendingETA == nil {
			if fallback := routeDurationDefault(pointerValue(parsed.Route)); fallback > 0 {
				pendingETA = &fallback
			}
		}
		callKey, keyErr := callKey(event)
		if keyErr != nil {
			return nil, keyErr
		}
		callID = codexCallUUID(callKey)
		withoutCall := make(map[string]any, len(input))
		for key, value := range input {
			if key != "call" {
				withoutCall[key] = value
			}
		}
		encoded, encodeErr := pythonJSON(withoutCall)
		if encodeErr != nil {
			return nil, encodeErr
		}
		sum := sha256.Sum256(encoded)
		inputHash = hex.EncodeToString(sum[:])
		updated["mode"], updated["call"], updated["taskId"], updated["route"] = mode, callID, *parsed.TaskID, *parsed.Route
		updated["jobModel"], updated["jobEffort"] = selection.Model, selection.Effort
		if text, ok := label.(string); ok && text != "" {
			updated["jobLabel"] = label
		}
		updated["jobPhase"] = *parsed.Phase
		if pendingETA != nil {
			updated["etaMinutes"] = *pendingETA
		}
		if worktreeStart {
			updated["worktree"] = true
			if root != nil {
				requested, _ := input["cwd"].(string)
				if requested == "" {
					requested = stringValue(event["cwd"])
				}
				launch := *root
				if requested != "" {
					candidate := requested
					if !filepath.IsAbs(candidate) {
						candidate = filepath.Join(*root, candidate)
					}
					if resolved, resolveErr := filepath.Abs(candidate); resolveErr == nil {
						if relative, relErr := filepath.Rel(*root, resolved); relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
							launch = resolved
						}
					}
				}
				updated["cwd"] = launch
			}
		}
	}
	return persistPreSuccess(event, runtimeRoot, root, live, parsed, selection, updated,
		callID, inputHash, pendingETA, worktreeReplyPath, isStart, reply, worktreeStart, worktreeReply, label, config, bypassMetadata)
}

func persistPreSuccess(event map[string]any, runtimeRoot string, repository *string, live map[string]any,
	parsed policy.OrchParseResult, selection CoderSelection, updated map[string]any,
	callID, inputHash string, pendingETA *int, worktreeReplyPath *string, isStart, reply, worktreeStart, worktreeReply bool, label any, config map[string]any, bypassMetadata map[string]any) (result map[string]any, retErr error) {
	phase, taskID, routeName := pointerValue(parsed.Phase), pointerValue(parsed.TaskID), pointerValue(parsed.Route)
	project := projectKey(repository)
	input, _ := event["tool_input"].(map[string]any)
	var priorThread *string
	if reply && live != nil {
		if id, ok := live["thread_id"].(string); ok {
			priorThread = &id
		}
	}
	reason := parsed.Reason
	reasonSource := (*string)(nil)
	if reason != nil {
		value := "explicit"
		reasonSource = &value
	}
	if reply && reason == nil && live != nil {
		if inherited, ok := live["reason"].(string); ok {
			reason = &inherited
			source := "inherited"
			reasonSource = &source
		}
	}
	var previousRoute, newRoute *string
	if live != nil {
		if oldRoute, ok := live["route"].(string); ok && oldRoute != "" && oldRoute != routeName {
			previousRoute, newRoute = &oldRoute, &routeName
		}
	}
	objective, criteria, grounded := parseTaskContract(stringValue(input["prompt"]), taskID, phase, parsed.Scope)
	if !grounded && reply && live != nil {
		if oldObjective, ok := live["task_objective"].(string); ok && oldObjective != "" {
			if oldCriteria, ok := live["acceptance_criteria"].(string); ok && oldCriteria != "" {
				objective, criteria = oldObjective, oldCriteria
				grounded, _ = live["task_contract_grounded"].(bool)
			}
		}
	}
	taskType := "unknown"
	if grounded {
		taskType = deriveTaskType(objective, criteria, phase)
	}
	observationTaskType := taskType
	if observationTaskType == "unknown" {
		observationTaskType = phase
	}
	var suspected any
	if derived := deriveTaskType(objective, criteria, phase); derived == "counting_status" || derived == "operation" {
		if derived != routeName {
			suspected = map[string]any{"declared_route": routeName, "derived_type": derived}
		}
	}
	fallbackFamily := (*string)(nil)
	if fallback := mapValue(config["fallback"]); fallback != nil && selection.Agent == stringValue(fallback["agent"]) && selection.Model == stringValue(fallback["model"]) {
		if family, ok := fallback["family"].(string); ok {
			fallbackFamily = &family
		}
	}
	profileValue := (*profile.ResolvedProfile)(nil)
	if repository != nil {
		resolved := profile.LoadProfile(*repository)
		profileValue = &resolved
	}
	readiness := any(map[string]any{"agent": "off"})
	if value, ok := config["readiness_check"]; ok {
		readiness = value
	}
	mechanicalEnabled := config["mechanical_opencode_enabled"] != false
	baseline, pending := BuildPendingCall(PendingCallInput{
		TaskID: taskID, ProjectKey: project, Phase: phase, Route: routeName, Model: selection.Model,
		Effort: selection.Effort, Label: stringValue(label), Agent: selection.Agent,
		FallbackFamily: fallbackFamily, Scope: parsed.Scope, Prompt: stringValue(updated["prompt"]), WorktreeReplyPath: worktreeReplyPath,
		TaskObjective: objective, AcceptanceCriteria: criteria, TaskContractGrounded: grounded,
		TaskType: taskType, SuspectedRouteMismatch: suspected, HasCodeMap: strings.Contains(stringValue(updated["prompt"]), "CODE_MAP="),
		Reason: reason, ETAMinutes: pendingETA, ReplyCall: reply, Repository: repository,
		ReasonSource: reasonSource, PreviousRoute: previousRoute, NewRoute: newRoute,
		ManifestBudgetSeconds: numberValue(config["manifest_budget_seconds"]), LiveThread: live,
		Profile: profileValue, ReadinessCheck: readiness, MechanicalOpenCodeEnabled: &mechanicalEnabled,
	})
	if agentID := stringValue(event["agent_id"]); agentID != "" {
		pending["dispatcherAgentId"] = agentID
	}
	if priorThread != nil {
		pending["prior_thread_id"] = *priorThread
		bypassMetadata["prior_thread_id"] = *priorThread
	}
	var jobID *string
	if isStart {
		pending["async_call"], pending["async_mode"], pending["async_input_hash"] = callID, stringValue(updated["mode"]), inputHash
		pending["async_updated_input"], pending["async_ready"] = updated, false
		pending["job_id"] = nil
		pending["session_id"], pending["tool_use_id"] = event["session_id"], event["tool_use_id"]
		if _, ok := updated["label"]; ok {
			pending["label"] = updated["label"]
		}
		if grounded && criteria != "" {
			updated["readinessGrounded"] = true
			updated["readinessCriteria"] = map[string]any{"objective": objective, "acceptance_criteria": criteria}
			readinessMap := mapValue(readiness)
			updated["readinessAgent"] = fallbackValue(stringValue(readinessMap["agent"]))
			updated["readinessModel"] = stringValue(readinessMap["model"])
			pending["async_updated_input"] = updated
		}
		digest := sha256.Sum256([]byte(callID))
		value := hex.EncodeToString(digest[:])
		jobID = &value
	}
	path, err := pendingPath(runtimeRoot, event)
	if err != nil {
		return nil, err
	}
	owner, err := callKey(event)
	if err != nil {
		return nil, err
	}
	var threadID *string
	if priorThread != nil {
		threadID = priorThread
	}
	session := ledger.BuildSessionRecord(taskID, threadID, routeName, selection.Model, selection.Effort, map[string]any{
		"agent": selection.Agent, "phase": phase, "scope": stringSliceAny(parsed.Scope), "pending_call_baseline": baseline,
		"reason": optionalString(reason), "label": label, "task_contract_grounded": grounded,
		"task_objective": objective, "acceptance_criteria": criteria, "profile_checks": anySlice(pending["profile_checks"]),
	})
	var leaseRoot string
	leased := false
	if (phase == "implementation" || phase == "plan") && repository != nil {
		leaseScope := parsed.Scope
		if phase == "plan" {
			leaseScope = []string{}
		}
		timeout := numberValue(updated["timeoutSeconds"])
		admitted := AdmitPending(PendingAdmission{RuntimeRoot: runtimeRoot, ProjectKey: project, OwnerKey: owner,
			TaskID: taskID, Pending: pending, Session: session, PendingPath: path, Scope: leaseScope, JobID: jobID,
			TimeoutSeconds: timeout, Now: float64(time.Now().UnixNano()) / 1e9,
			ReadOnly: phase == "plan", AllowOverlap: worktreeStart || worktreeReply, HotPathQueue: worktreeStart,
			ThreadID: threadID, Repository: repository, OnAdmission: func(status, code string) {
				if code == "SCOPE_LEASE_CONFLICT" {
					preEmit(runtimeRoot, event, repository, taskID, project, "SCOPE_CONFLICT_OUTCOME", map[string]any{"action": "denied", "job_id": observationJobID(jobID)})
				}
				if code == "LEASE_QUEUED" {
					updated["leaseQueued"] = true
					preEmit(runtimeRoot, event, repository, taskID, project, "SCOPE_CONFLICT_OUTCOME", map[string]any{"action": "queued", "job_id": observationJobID(jobID)})
				}
			}})
		if admitted.Status != "OK" {
			if admitted.Code == "SCOPE_LEASE_CONFLICT" {
				preEmit(runtimeRoot, event, repository, taskID, project, "ROUTING_PROTOCOL_VIOLATION", map[string]any{"violation_code": admitted.Code})
				command := "purser-hook"
				if executable, err := os.Executable(); err == nil && executable != "" {
					command = `"` + executable + `"`
				}
				return denyPre(admitted.Code, fmt.Sprintf("Scope is leased by task %s. To release a stuck lease, run: %s leases --release <task-id>", admitted.TaskID, command)), nil
			}
			switch admitted.Code {
			case "LEASE_ADMISSION_UNCERTAIN", "LEASE_OWNER_CONFLICT", "LEASE_INVALID":
				preEmit(runtimeRoot, event, repository, taskID, project, "ROUTING_PROTOCOL_VIOLATION", map[string]any{"violation_code": admitted.Code})
				return denyPre(admitted.Code, "Scope ownership could not be established."), nil
			default:
				return nil, errors.New(admitted.Code)
			}
		}
		pending = admitted.Pending
		leased, leaseRoot = true, filepath.Join(runtimeRoot, "projects", project)
	} else {
		admitted := AdmitPending(PendingAdmission{RuntimeRoot: runtimeRoot, ProjectKey: project, OwnerKey: owner,
			TaskID: taskID, Pending: pending, Session: session, PendingPath: path, Now: float64(time.Now().UnixNano()) / 1e9,
			ThreadID: threadID, Repository: repository, SkipLease: true})
		if admitted.Status != "OK" {
			return nil, errors.New(admitted.Code)
		}
		pending = admitted.Pending
	}
	pendingWritten := true
	defer func() {
		recovered := recover()
		if recovered != nil {
			retErr = fmt.Errorf("panic: %v", recovered)
		}
		if retErr != nil {
			if leased {
				_ = lease.New(leaseRoot).Release(owner, float64(time.Now().UnixNano())/1e9)
			}
			if pendingWritten {
				_ = os.Remove(path)
			}
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	if isStart {
		pending["async_ready"] = true
		if err := writeAsyncReady(path, pending); err != nil {
			return nil, err
		}
	}
	var eta any
	if parsed.ETAMinutes != nil {
		eta = *parsed.ETAMinutes
	}
	var observedReasonSource any
	if reasonSource != nil {
		observedReasonSource = *reasonSource
	}
	obs := map[string]any{"phase": phase, "route": routeName, "model": selection.Model, "model_source": selection.ModelSource,
		"effort": selection.Effort, "routing_enforced": true, "fresh_thread": !reply, "reply_call": reply,
		"reason": optionalString(reason), "eta_minutes": eta, "worker_prompt_chars": len([]rune(stringValue(updated["prompt"]))),
		"scope_path_count": len(parsed.Scope),
		"reason_source":    observedReasonSource, "previous_route": optionalString(previousRoute), "new_route": optionalString(newRoute),
		"task_type": observationTaskType, "suspected_route_mismatch": suspected,
		"agent": selection.Agent, "provider": provider(selection.Agent, selection.Model)}
	preEmit(runtimeRoot, event, repository, taskID, project, "TASK_ROUTE_SELECTED", obs)
	started := make(map[string]any, len(obs)+1)
	for key, value := range obs {
		started[key] = value
	}
	started["implementer"] = selection.Agent
	preEmit(runtimeRoot, event, repository, taskID, project, "IMPLEMENTER_STARTED", started)
	if sessionID, _ := event["session_id"].(string); sessionID != "" {
		started := startActiveTask(runtimeRoot, sessionID, taskID, project, stringValue(event["tool_use_id"]), stringValue(event["transcript_path"]), claudeProjectsRoot(), time.Now().UTC())
		if started["status"] == "UNKNOWN" {
			recordObservationHealth(runtimeRoot, "DEGRADED", "CLAUDE_TRANSCRIPT_PARSER_FAILED")
		}
	}
	contextio.AssociateTurn(runtimeRoot, stringValue(event["session_id"]), taskID, project)
	if reply && !isStart {
		preEmit(runtimeRoot, event, repository, taskID, project, "CLAUDE_DECISION_RECORDED", map[string]any{
			"decision": "send_back", "task_type": observationTaskType, "source": map[string]any{"kind": "reply", "job_id": owner},
		})
	}
	pendingWritten = false
	leased = false
	hook := map[string]any{"hookEventName": "PreToolUse", "updatedInput": updated}
	return map[string]any{"hookSpecificOutput": hook,
		"systemMessage": fmt.Sprintf("Purser: route=%s -> %s / %s", fallbackValue(routeName), fallbackValue(selection.Model), fallbackValue(selection.Effort))}, nil
}

func nonTestScopeCount(scope []string) int {
	count := 0
	for _, entry := range scope {
		clean := strings.ReplaceAll(entry, "\\", "/")
		base := strings.ToLower(clean[strings.LastIndex(clean, "/")+1:])
		isTest := strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")
		if !isTest {
			for _, segment := range strings.Split(strings.ToLower(clean), "/") {
				if segment == "tests" || segment == "testdata" {
					isTest = true
					break
				}
			}
		}
		if !isTest {
			count++
		}
	}
	return count
}

func hasCompletedPlan(stateRoot, taskID string) bool {
	entries, err := os.ReadDir(filepath.Join(stateRoot, "plans"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		planID := name
		if dash := strings.LastIndex(name, "-"); dash >= 0 {
			key := name[dash+1:]
			if len(key) == 64 {
				if _, err := hex.DecodeString(key); err == nil {
					planID = name[:dash]
				}
			}
		}
		if planID != "" && (taskID == planID || strings.HasPrefix(taskID, planID+"-")) {
			return true
		}
	}
	return false
}

func inputLabel(event map[string]any) any {
	input, _ := event["tool_input"].(map[string]any)
	return input["label"]
}

func planMilestoneLabel(taskID string, label any) any {
	text, ok := label.(string)
	if !ok || text == "" {
		return label
	}
	match := planMilestoneTaskID.FindStringSubmatch(taskID)
	if len(match) != 4 {
		return label
	}
	suffix := fmt.Sprintf(" [%s-%s/%s]", match[1], match[2], match[3])
	if strings.HasSuffix(text, suffix) {
		return label
	}
	return text + suffix
}

func stringSliceAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func anySlice(value any) any {
	switch values := value.(type) {
	case []map[string]any:
		out := make([]any, len(values))
		for i := range values {
			out[i] = values[i]
		}
		return out
	case []string:
		return stringSliceAny(values)
	default:
		return value
	}
}

func observationJobID(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func preEmit(root string, event map[string]any, repository *string, taskID, project, eventType string, payload map[string]any) {
	defer func() {
		if recover() != nil {
			recordObservationHealth(root, "DEGRADED", "TELEMETRY_ENTRYPOINT_FAILURE")
		}
	}()
	envelope := map[string]any{"session_id": event["session_id"], "project_key": project, "agent_id": event["agent_id"], "agent_type": event["agent_type"]}
	if repository != nil {
		envelope["repo"] = filepath.Base(*repository)
		head, err := os.ReadFile(filepath.Join(*repository, ".git", "HEAD"))
		if err == nil && len(head) <= 4096 {
			value := strings.TrimSpace(string(head))
			if strings.HasPrefix(value, "ref: refs/heads/") && len(value) > len("ref: refs/heads/") {
				envelope["branch"] = strings.TrimPrefix(value, "ref: refs/heads/")
			}
		}
	} else {
		if event["repo"] != nil {
			envelope["repo"] = event["repo"]
		}
		if event["branch"] != nil {
			envelope["branch"] = event["branch"]
		}
	}
	if observation, err := buildObservationAtStateRoot(eventType, envelope, payload, taskID, root); err == nil {
		telemetryRoot := filepath.Join(root, "telemetry")
		if code, err := persistObservation(telemetryRoot, observation); err == nil {
			updateObservationTaskBestEffort(telemetryRoot, taskID, project)
		} else {
			recordObservationHealth(root, "DEGRADED", code)
		}
	} else {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
	}
}

// RecordUnfetchedResult mirrors telemetry.emit for cleanup's result-not-fetched marker.
func RecordUnfetchedResult(root string, projectKey, route any) {
	defer func() {
		if recover() != nil {
			recordObservationHealth(root, "DEGRADED", "TELEMETRY_ENTRYPOINT_FAILURE")
		}
	}()
	payload := map[string]any{"route": route, "code": "RESULT_NOT_FETCHED"}
	envelope := map[string]any{"project_key": projectKey}
	if event, err := buildObservationAtStateRoot("CODEX_RESULT_UNFETCHED", envelope, payload, nil, root); err == nil {
		if code, err := persistObservation(filepath.Join(root, "telemetry"), event); err != nil {
			recordObservationHealth(root, "DEGRADED", code)
		}
	} else {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
	}
}

// RecordClaudeDecision mirrors the best-effort telemetry emitted by the Python bridge.
func RecordClaudeDecision(root string, job map[string]any, decision, kind, sourceJobID string, reviewerCounts ...any) {
	task, ok := job["taskId"].(string)
	if !ok || task == "" {
		return
	}
	phase, _ := job["jobPhase"].(string)
	if phase == "" {
		phase, _ = job["phase"].(string)
	}
	if phase != "implementation" && phase != "operation" && phase != "plan" {
		phase = "unknown"
	}
	var project any
	if repo, ok := job["repoRoot"].(string); ok && repo != "" {
		project = projectKey(&repo)
	}
	envelope := map[string]any{"session_id": nil, "project_key": project, "repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil}
	payload := map[string]any{"decision": decision, "task_type": phase, "source": map[string]any{"kind": kind, "job_id": sourceJobID}}
	for i, field := range []string{"reviewer1_valid_count", "reviewer1_false_count"} {
		if i >= len(reviewerCounts) {
			break
		}
		if count, ok := nonnegativeInteger(reviewerCounts[i]); ok {
			payload[field] = count
		}
	}
	if event, err := buildObservationAtStateRoot("CLAUDE_DECISION_RECORDED", envelope, payload, task, root); err == nil {
		_, _ = persistObservation(filepath.Join(root, "telemetry"), event)
	}
}

func nonnegativeInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		if number >= 0 {
			return int64(number), true
		}
	case int64:
		if number >= 0 {
			return number, true
		}
	case json.Number:
		parsed, err := number.Int64()
		if err == nil && parsed >= 0 {
			return parsed, true
		}
	}
	return 0, false
}

func provider(agent, model string) any {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[:i]
	}
	switch agent {
	case "codex":
		return "openai"
	case "agy":
		return "google"
	}
	return nil
}
func fallbackValue(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
func numberValue(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func intValue(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
func codexCallUUID(name string) string {
	namespace := []byte{0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	hash := sha1.Sum(append(namespace, []byte(name)...))
	raw := hash[:16]
	raw[6] = raw[6]&0x0f | 0x50
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func routeDurationDefault(route string) int {
	switch route {
	case "mechanical":
		return 3
	case "subtle", "normal":
		return 5
	case "consequential":
		return 8
	case "escalated":
		return 12
	case "operation":
		return 3
	case "plan_standard":
		return 8
	}
	return 0
}

func preThreadWorktreePath(runtimeRoot string, threadID any) string {
	id, ok := threadID.(string)
	if !ok || !validThreadID.MatchString(id) {
		return ""
	}
	root := os.Getenv("PURSER_CODEX_SHIM_STATE")
	if root == "" {
		root = filepath.Join(runtimeRoot, "codex_exec_mcp", "threads")
	}
	sum := sha256.Sum256([]byte(id))
	data, err := os.ReadFile(filepath.Join(root, hex.EncodeToString(sum[:])+".json"))
	if err != nil {
		return ""
	}
	var record map[string]any
	if json.Unmarshal(data, &record) != nil || record["thread_id"] != id {
		return ""
	}
	if _, ok := record["cwd"].(string); !ok {
		return ""
	}
	if _, ok := record["config_args"].([]any); !ok {
		return ""
	}
	path, _ := record["worktreePath"].(string)
	return path
}

func pythonJSON(value any) ([]byte, error) {
	compact, err := canonicalJSON(value)
	if err != nil {
		return nil, err
	}
	formatted := make([]byte, 0, len(compact)+16)
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
	return formatted, nil
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
	pluginRoot := pluginroot.Root()
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

const preCodeMapExcerptMaxBytes = 6000

const prePlanShapeMarker = "Required plan shape (Purser):"

func appendPrePlanShape(prompt, phase string) string {
	if phase != "plan" {
		return prompt
	}
	if strings.Contains(prompt, prePlanShapeMarker) {
		return prompt
	}
	block := `Required plan shape (Purser):
Write Markdown with a leading Summary: section of at most 4000 characters, then sections Goal and boundaries, Current evidence (observed facts vs labelled assumptions, with paths/symbols), Decision (one recommendation and brief alternatives), Milestones (at most 5; each has bounded scope, observable acceptance, validation, dependency, touched files/dirs, and parallel group; same-group paths are disjoint), Risks/open decisions (at most 5, each with an owner or blocking question), and Implementation handoff (first task's paths and acceptance). Read-only: no code edits.`
	return strings.TrimRight(prompt, " \t\r\n") + "\n\n" + block
}

func appendPreCodeMap(prompt, path, status string, scope ...[]string) string {
	if strings.Contains(prompt, "CODE_MAP=") {
		return prompt
	}
	line := "CODE_MAP=" + path + " STATUS=" + status
	if status == "FRESH" || status == "STALE" {
		if len(scope) > 0 && len(scope[0]) > 0 {
			if data, err := os.ReadFile(path); err == nil {
				if excerpt := scopedCodeMapExcerpt(data, scope[0], line+" (scoped excerpt below)\n", path); excerpt != "" {
					line = strings.TrimSuffix(excerpt, "\n")
				}
			}
		}
	}
	return strings.TrimRight(prompt, " \t\r\n") + "\n" + line
}

func scopedCodeMapExcerpt(data []byte, scope []string, header, path string) string {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var sections []string
	for i := 0; i < len(lines); {
		line := lines[i]
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "Parser:") || strings.HasPrefix(line, "Covered languages:") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			i++
			continue
		}
		start := i
		i++
		for i < len(lines) && (strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t")) {
			i++
		}
		if codeMapPathInScope(line, scope) {
			sections = append(sections, strings.Join(lines[start:i], "\n"))
		}
	}
	if len(sections) == 0 {
		return ""
	}

	for keep := len(sections); keep >= 0; keep-- {
		body := strings.Join(sections[:keep], "\n")
		if body != "" {
			body += "\n"
		}
		if keep < len(sections) {
			body += fmt.Sprintf("... (%d more files in %s)\n", len(sections)-keep, path)
		}
		candidate := header + body
		if len(candidate) <= preCodeMapExcerptMaxBytes {
			return candidate
		}
	}
	return ""
}

func codeMapPathInScope(file string, scope []string) bool {
	file = filepath.ToSlash(filepath.Clean(file))
	for _, entry := range scope {
		entry = filepath.ToSlash(filepath.Clean(strings.TrimSpace(entry)))
		if entry == "." || file == entry || strings.HasPrefix(file, strings.TrimSuffix(entry, "/")+"/") {
			return true
		}
	}
	return false
}
