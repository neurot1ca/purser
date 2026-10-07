package enforce

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/lease"
	"purser-hook/internal/models"
	"purser-hook/internal/obsidian"
	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

// PostToolUse dispatches the Go Post lane. The Python hook remains the process
// matrix oracle until its caller is migrated in pass 116b.
func PostToolUse(event map[string]any, root string) (response map[string]any, failure error) {
	if event["hook_event_name"] != "PostToolUse" {
		return nil, nil
	}
	tool := stringValue(event["tool_name"])
	if tool == "mcp__codex-main__codex_apply" || strings.HasSuffix(tool, "__codex_apply") {
		return postApply(event, defaultPreStateRoot(root)), nil
	}
	if !codexTools[tool] {
		return map[string]any{}, nil
	}
	root = defaultPreStateRoot(root)
	var err error
	defer func() {
		if value := recover(); value != nil {
			failure = fmt.Errorf("panic: %v", value)
			response = postInternalOutput(event, root, failure)
		}
	}()
	if strings.HasSuffix(tool, "__codex_start") {
		response, err = postStartDispatch(event, root)
	} else if strings.HasSuffix(tool, "__codex_result") || strings.HasSuffix(tool, "__codex_cancel") {
		response, err = postAsyncDispatch(event, root)
	} else {
		response, err = postSyncDispatch(event, root)
	}
	if err != nil {
		return postInternalOutput(event, root, err), err
	}
	return response, nil
}

func postInternalOutput(event map[string]any, root string, cause error) map[string]any {
	bundle := baseBundle(safeError(cause))
	if root != "" {
		if path, err := persistRawOutput(root, event, event["tool_response"]); err == nil {
			bundle["raw_output_path"] = path
		}
		_ = writePostDiagnostic(root, event, cause)
	}
	field := "updatedToolOutput"
	if config, err := loadPreConfig(); err == nil {
		bundle["reviewer1_model_requested"] = config["reviewer1_model_requested"]
		if value := stringValue(config["post_tool_output_field"]); value != "" {
			field = value
		}
		if root != "" {
			if compact, _, finalizeErr := finalizePostArtifacts(root, event, bundle, 8000,
				postReportLimit(config, nil)); finalizeErr == nil {
				bundle = compact
			}
		}
	}
	response, err := replacement(bundle, field)
	if err != nil {
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "updatedToolOutput": []any{map[string]any{"type": "text", "text": `{}`}}}}
	}
	return response
}

func postSyncDispatch(event map[string]any, root string) (map[string]any, error) {
	intake, early, err := postIntake(event, root)
	if err != nil {
		return early, err
	}
	if early != nil {
		PostImplementerObservations(event, intake.Pending, intake.Bundle, root)
		return early, nil
	}
	pending, bundle, resolved := intake.Pending, intake.Bundle, intake.Profile
	loaded, err := loadPreConfig()
	if err != nil {
		return nil, err
	}
	started := time.Now()
	repoRoot := stringValue(pending["repo_root"])
	project := pendingProjectKey(pending)
	if resolved.ForceFullReview {
		code, msg := "PROFILE_FORCES_FULL", "The resolved profile forces FULL Claude review."
		if resolved.UserStatus == "rejected" || resolved.ProjectStatus == "rejected" {
			code, msg = "PROFILE_REJECTED", stringValue(resolved.FirstWarning)
			if msg == "" {
				msg = "a profile tier was rejected"
			}
		}
		bundle["findings"] = appendPostFinding(bundle["findings"], code, msg)
	}
	if resolved.Stored && resolved.Configured != nil && !*resolved.Configured {
		bundle["profile"] = "not_configured"
	}
	if resolved.Reviewer == "none" {
		bundle["reviewer1_skipped_reason"] = "REVIEWER_DISABLED"
		bundle["findings"] = appendPostFinding(bundle["findings"], "REVIEWER_DISABLED", "no independent reviewer configured; FULL review required")
	}
	if stringValue(pending["phase"]) == "plan" {
		if _, err = storePostPlan(root, event, pending["task_id"], intake.Report, bundle); err != nil {
			return nil, err
		}
	}
	if pendingAgent(pending) == "codex" {
		bundle["codex_quota"] = postReadQuota(intake.ThreadID)
	}
	if stringValue(pending["phase"]) != "plan" {
		worker := parseWorkerValidation(intake.Report)
		bundle["worker_validation"] = worker
	}
	var deltaBundle map[string]any
	foreignOwners := []map[string]any{}
	if owner := stringValue(pending["lease_owner"]); owner != "" {
		var historyErr error
		foreignOwners, historyErr = lease.New(filepath.Join(root, "projects", project)).OverlappingOwners(owner, float64(time.Now().UnixNano())/1e9)
		if historyErr != nil {
			bundle["force_full_review"] = true
			bundle["findings"] = appendPostFinding(bundle["findings"], "LEASE_HISTORY_UNAVAILABLE", "Scope lease history could not be verified.")
			foreignOwners = nil
		}
	}
	deltaBundle, err = assemblePostDelta(repoRoot, pending, bundle, resolved.ProtectedPaths, foreignOwners)
	if err != nil {
		return nil, err
	}
	delta, _ := deltaBundle["delta"].(map[string]any)
	gateConfig := loaded
	checks := postConfiguredChecks(pending)
	profileForcesFull := resolved.ForceFullReview
	routeKnown := stringValue(pending["route"]) != ""
	gateDeadline := time.Duration(numberOr(loaded["hook_checks_deadline_seconds"], 30)) * time.Second
	schedulePostReview(repoRoot, pending, bundle, resolved, checks, gateConfig, gateDeadline,
		resolved.Reviewer != "none", routeKnown)
	classification := mapValue(bundle["concurrent_writer"])
	if stringValue(bundle["reviewer1_skipped_reason"]) == "" && stringValue(bundle["claude_review_mode"]) == review.Full {
		budget := time.Duration(numberOr(loaded["max_total_hook_budget_seconds"], 240)) * time.Second
		bridge, bridgeErr := postReviewerDispatch(pluginRoot(), root, pending, bundle, resolved, loaded, delta, classification, started, budget)
		repository := repoRoot
		recordPostReviewerObservations(root, event, optionalRepository(&repository), stringValue(pending["task_id"]), project,
			postReviewerStarted(bundle), stringValue(bridge.Attributes["provider"]), stringValue(loaded["reviewer1_model_requested"]),
			stringValue(bundle["reviewer1_skipped_reason"]), bridge, bridgeErr, time.Since(started), len(postReviewerFindings(bridge.Attributes["findings"])))
		if bridgeErr != nil {
			return nil, bridgeErr
		}
		applyPostReviewerOutcome(root, event, pending, bundle, delta, classification, bridge, routeKnown,
			routeInList(loaded["always_full_routes"], stringValue(pending["route"])))
	}
	if stringValue(bundle["reviewer1_skipped_reason"]) != "" {
		recordPostReviewerObservations(root, event, optionalRepository(&repoRoot), stringValue(pending["task_id"]), project,
			false, "", stringValue(loaded["reviewer1_model_requested"]), stringValue(bundle["reviewer1_skipped_reason"]), review.BridgeOutcome{}, nil, 0, -1)
	}
	if err = finishPostReviewDelta(repoRoot, root, event, pending, bundle, resolved, loaded, delta, classification, foreignOwners, started); err != nil {
		return nil, err
	}
	delta, classification = mapValue(bundle["delta"]), mapValue(bundle["concurrent_writer"])
	gate := review.GateDecision{Run: bundle["reviewer1_skipped_reason"] == nil,
		Reason: stringValue(bundle["reviewer1_skipped_reason"])}
	finalizePostReview(bundle, pending, loaded, gate, classification, routeKnown,
		bundle["force_full_review"] == true, mapValue(bundle["hook_checks"])["status"] == "FAIL", profileForcesFull)
	PostImplementerObservations(event, pending, bundle, root)
	PostMechanicalLearning(loaded, root, pluginRoot(), project, pending, bundle)
	if err = finishPostSession(root, event, pending, bundle, intake.ThreadID, repoRoot, false, float64(time.Now().UnixNano())/1e9, numberOr(loaded["manifest_budget_seconds"], 3)); err != nil {
		return nil, err
	}
	recordPostCompletion(root, event, repoRoot, bundle)
	if pending["has_code_map"] == true && len(anyStringList(bundle["changed_paths"])) > 0 {
		bundle["code_map"] = "STALE"
	}
	compact, _, err := finalizePostArtifacts(root, event, bundle, 8000, postReportLimit(loaded, pending["phase"]))
	if err != nil {
		return nil, err
	}
	response, err := replacement(compact, stringValue(loaded["post_tool_output_field"]))
	if err == nil {
		appendPostQuotaSystemMessage(response, bundle["codex_quota"])
		postGearAdvice(root, event, pending, loaded, response, numberOr(event["duration_ms"], 0), "")
	}
	if err == nil && stringValue(pending["async_call"]) == "" {
		response = appendPostDispatchReceipt(response, root, event, pending, repoRoot)
	}
	return response, err
}

func postReviewerStarted(bundle map[string]any) bool {
	attempts, _ := bundle["reviewer1_attempts"].([]any)
	for _, raw := range attempts {
		outcome := stringValue(mapValue(raw)["outcome"])
		if outcome == "COMPLETED" || outcome == "UNAVAILABLE" {
			return true
		}
	}
	return bundle["reviewer1_skipped_reason"] == nil
}

var postReviewerDispatch = runPostReviewer
var postMicroRepairDispatch postRepairRunner

func postConfiguredChecks(pending map[string]any) []profile.Check {
	checks, _ := pending["profile_checks"].([]any)
	result := make([]profile.Check, 0, len(checks))
	for _, raw := range checks {
		item := mapValue(raw)
		argv, argvOK := postStringList(item["argv"])
		when, whenOK := postStringList(item["when"])
		if item == nil || !argvOK || !whenOK {
			continue
		}
		timeout, _ := pythonInt(item["timeout_seconds"])
		result = append(result, profile.Check{Name: stringValue(item["name"]), Argv: argv, When: when, TimeoutSeconds: timeout})
	}
	return result
}

func postStartDispatch(event map[string]any, root string) (map[string]any, error) {
	loaded, err := loadPreConfig()
	if err != nil {
		return nil, err
	}
	pending, pendingErr := readPostPending(pendingCandidates(root, event))
	adapted := make(map[string]any, len(event)+1)
	for key, value := range event {
		adapted[key] = value
	}
	adapted["post_tool_output_field"] = loaded["post_tool_output_field"]
	response, err := postStart(adapted, root)
	if err != nil {
		return nil, err
	}
	if pendingErr != nil {
		return response, nil
	}
	return appendPostDispatchReceipt(response, root, event, pending, stringValue(pending["repo_root"])), nil
}

func optionalRepository(value *string) *string {
	if *value == "" {
		return nil
	}
	return value
}

func numberOr(value any, fallback float64) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		if parsed, err := n.Float64(); err == nil {
			return parsed
		}
	default:
	}
	return fallback
}

func finishPostReviewDelta(repoRoot, root string, event, pending, bundle map[string]any, resolved postProfile,
	loaded map[string]any, delta, classification map[string]any, foreignOwners []map[string]any, started time.Time) error {
	findings := []postRepairFinding{}
	for _, finding := range postReviewerFindings(bundle["findings"]) {
		findings = append(findings, postRepairFinding{Code: stringValue(finding["code"]), Message: stringValue(finding["message"])})
	}
	result := stringValue(bundle["reviewer1_result"])
	if stringValue(pending["agent"]) == "codex" && loaded["micro_repair_enabled"] != false && result == "CHANGES_REQUIRED" && stringValue(bundle["thread_id"]) != "" {
		_, attempted := attemptPostMicroRepair(bundle, "codex", result, stringValue(bundle["thread_id"]), repoRoot,
			findings, postScope(pending), true, int(numberOr(loaded["micro_repair_timeout_seconds"], 60)), time.Until(started.Add(time.Duration(numberOr(loaded["max_total_hook_budget_seconds"], 240))*time.Second)), postMicroRepairDispatch)
		if attempted {
			delta = recomputePostRepairDelta(repoRoot, pending, bundle, resolved.ProtectedPaths, foreignOwners, mapValue(bundle["micro_repair"])["status"] == "SUCCEEDED")
			bundle["delta"] = delta
			if mapValue(bundle["micro_repair"])["status"] == "SUCCEEDED" {
				if err := rereviewPostRepair(root, root, pending, bundle, resolved, loaded, delta, classification, started, time.Duration(numberOr(loaded["max_total_hook_budget_seconds"], 240))*time.Second); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func postApply(event map[string]any, root string) map[string]any {
	input := mapValue(event["tool_input"])
	jobID := stringValue(input["jobId"])
	if jobID == "" {
		return map[string]any{}
	}
	index, err := readAsyncObject(filepath.Join(root, "async_jobs", jobID+".json"))
	if err != nil {
		return map[string]any{}
	}
	job := asyncJob(root, index)
	if job == nil || (job["applyState"] != "applied" && job["applyState"] != "conflict") {
		return map[string]any{}
	}
	message, _ := PostProjectJobBoard(root, stringValue(index["project_key"]), time.Now())
	context := message
	if guidance := postPendingGuidance(root, event, stringValue(job["repoRoot"])); guidance != "" {
		if context != "" {
			context += "\n"
		}
		context += guidance
	}
	response := map[string]any{}
	if context != "" {
		response["hookSpecificOutput"] = map[string]any{"hookEventName": "PostToolUse", "additionalContext": context}
	}
	return response
}

func postAsyncDispatch(event map[string]any, root string) (map[string]any, error) {
	repoRoot := stringValue(event["repo_root"])
	lock, index, pending, original, cached, err := claimAsyncResult(root, event, repoRoot)
	if err != nil {
		loaded, loadErr := loadPreConfig()
		if loadErr != nil {
			return nil, loadErr
		}
		response, replaceErr := replacement(map[string]any{"status": "ALREADY_CONSUMED_OR_INTERRUPTED", "claude_review_mode": "FULL", "ambiguous_outcome": true}, stringValue(loaded["post_tool_output_field"]))
		return response, replaceErr
	}
	defer func() { releaseTaskUsageLock(lock); _ = lock.Close() }()
	if cached != nil {
		response, _ := cached.(map[string]any)
		job := asyncJob(root, index)
		if !(job != nil && stringValue(job["worktreePath"]) != "" && job["applyState"] != "applied") {
			key, project := stringValue(index["call_key"]), stringValue(index["project_key"])
			if asyncHash.MatchString(key) {
				result := lease.New(filepath.Join(root, "projects", project)).Release(key, float64(time.Now().UnixNano())/1e9)
				if result.Status != "OK" && result.Code != "LEASE_OWNER_MISSING" {
					return nil, fmt.Errorf("scope lease cleanup failed: %s", result.Code)
				}
				if err := os.Remove(filepath.Join(root, "pending", key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("pending cleanup failed: %w", err)
				}
			}
		}
		if response != nil && len(response) > 0 {
			contextio.ClearCompletedGearAdviceRecovery(root, stringValue(index["project_key"]), stringValue(index["jobId"]))
			return response, nil
		}
		loaded, loadErr := loadPreConfig()
		if loadErr != nil {
			return nil, loadErr
		}
		return replacement(map[string]any{"status": "ALREADY_CONSUMED_OR_INTERRUPTED", "claude_review_mode": "FULL", "ambiguous_outcome": true}, stringValue(loaded["post_tool_output_field"]))
	}
	if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_cancel") {
		if result, handled, failure := finalizeAsyncFailure(root, event, index, pending); handled || failure != nil {
			if failure == nil && result != nil {
				context := stringValue(mapValue(result["hookSpecificOutput"])["additionalContext"])
				field := "updatedToolOutput"
				if config, configErr := loadPreConfig(); configErr == nil && stringValue(config["post_tool_output_field"]) != "" {
					field = stringValue(config["post_tool_output_field"])
				}
				result, failure = replacement(map[string]any{"state": "FAILED", "ambiguous_outcome": true,
					"claude_review_mode": review.Full}, field)
				if failure == nil {
					output := mapValue(result["hookSpecificOutput"])
					output["additionalContext"], output["hookEventName"] = context, "PostToolUse"
				}
				index["response"] = result
				_ = atomicAdmissionJSON(filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json"), index)
			}
			return result, failure
		}
		loaded, loadErr := loadPreConfig()
		if loadErr != nil {
			return nil, loadErr
		}
		return replacement(map[string]any{"status": "ALREADY_CONSUMED_OR_INTERRUPTED", "claude_review_mode": "FULL", "ambiguous_outcome": true}, stringValue(loaded["post_tool_output_field"]))
	}
	loaded, err := loadPreConfig()
	if err != nil {
		return nil, err
	}
	if recovered, handled, recoveryErr := recoverAsyncFinalizing(root, event, index, pending, 8000,
		postReportLimit(loaded, pending["phase"])); handled || recoveryErr != nil {
		return recovered, recoveryErr
	}
	resolved := postProfileFromPending(pending, loaded)
	response, err := finishAsyncSuccess(root, event, index, pending, original, stringValue(pending["repo_root"]), loaded,
		resolved, nil, loaded, 8000, postReportLimit(loaded, pending["phase"]), 240*time.Second)
	if err != nil {
		if failed, handled, failErr := finalizeAsyncFailure(root, event, index, pending); handled {
			if failErr == nil && failed != nil {
				failed = appendAsyncProjectStatus(root, event, index, pending, failed)
			}
			return failed, failErr
		}
		_ = writePostDiagnostic(root, event, err)
		response, failure := failAsyncFinalization(root, index, stringValue(loaded["post_tool_output_field"]))
		return response, failure
	}
	if bundle := postResponseBundle(response, loaded); bundle != nil {
		PostImplementerObservations(original, pending, bundle, root)
		PostMechanicalLearning(loaded, root, pluginRoot(), pendingProjectKey(pending), pending, bundle)
		queuePostCompletionReceipt(root, original, stringValue(pending["repo_root"]), bundle)
	}
	response = appendPostDispatchReceipt(response, root, event, pending, stringValue(pending["repo_root"]))
	index["response"] = response
	if err = atomicAdmissionJSON(filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json"), index); err != nil {
		return nil, err
	}
	contextio.ClearCompletedGearAdviceRecovery(root, stringValue(index["project_key"]), stringValue(index["jobId"]))
	return response, err
}

func failAsyncFinalization(root string, index map[string]any, outputField string) (map[string]any, error) {
	if outputField == "" {
		outputField = "updatedToolOutput"
	}
	response, err := replacement(map[string]any{"status": "FAILED", "errorCode": "INTERNAL_POST_ERROR",
		"message": "async result could not be finalized", "ambiguous_outcome": true, "claude_review_mode": review.Full}, outputField)
	if err != nil {
		return nil, err
	}
	index["stage"], index["errorCode"], index["claude_review_mode"], index["response"] = "FAILED", "INTERNAL_POST_ERROR", review.Full, response
	if err = atomicAdmissionJSON(filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json"), index); err != nil {
		return nil, err
	}
	return response, nil
}

func postResponseBundle(response, loaded map[string]any) map[string]any {
	field := stringValue(loaded["post_tool_output_field"])
	output := mapValue(response["hookSpecificOutput"])[field]
	items, _ := output.([]any)
	if len(items) == 0 {
		return nil
	}
	textValue := stringValue(mapValue(items[0])["text"])
	var bundle map[string]any
	if json.Unmarshal([]byte(textValue), &bundle) != nil {
		return nil
	}
	return bundle
}

func appendAsyncProjectStatus(root string, event, index, pending, response map[string]any) map[string]any {
	board, _ := PostProjectJobBoard(root, stringValue(index["project_key"]), time.Now())
	guidance := postPendingGuidance(root, event, stringValue(pending["repo_root"]))
	context := strings.TrimSpace(strings.Join([]string{board, guidance}, "\n"))
	if context != "" {
		output := mapValue(response["hookSpecificOutput"])
		if output == nil {
			output = map[string]any{"hookEventName": "PostToolUse"}
			response["hookSpecificOutput"] = output
		}
		if prior := stringValue(output["additionalContext"]); prior != "" {
			context = prior + "\n" + context
		}
		output["additionalContext"] = context
	}
	index["response"] = response
	_ = atomicAdmissionJSON(filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json"), index)
	return response
}

func recordPostCompletion(root string, event map[string]any, repo string, bundle map[string]any) {
	if repo == "" {
		return
	}
	queuePostCompletionReceipt(root, event, repo, bundle)
}

func appendPostDispatchReceipt(response map[string]any, root string, event, pending map[string]any, repo string) map[string]any {
	line := PostDispatchReceipt(pending, nil)
	if line != "" && stringValue(pending["async_call"]) == "" {
		queuePostReceipt(root, event, repo, line)
	}
	context := line
	if stringValue(pending["async_call"]) != "" {
		board, _ := PostProjectJobBoard(root, pendingProjectKey(pending), time.Now())
		if board != "" {
			if context != "" {
				context += "\n"
			}
			context += board
		}
	}
	guidance := postPendingGuidance(root, event, repo)
	if guidance != "" {
		if context != "" {
			context += "\n"
		}
		context += guidance
	}
	if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_start") &&
		stringValue(mapValue(response["hookSpecificOutput"])["additionalContext"]) == "" &&
		pending["async_ready"] == true {
		if current, newer, ok := models.DiscoverCachedCodexVersion(); ok {
			upgrade := contextio.ModelVersionGuidance(current, newer)
			if context != "" {
				context += "\n"
			}
			context += upgrade
		}
	}
	if context == "" {
		return response
	}
	output := mapValue(response["hookSpecificOutput"])
	if output == nil {
		output = map[string]any{"hookEventName": "PostToolUse"}
		response["hookSpecificOutput"] = output
	}
	if prior := stringValue(output["additionalContext"]); prior != "" {
		output["additionalContext"] = prior + "\n" + context
	} else {
		output["additionalContext"] = context
	}
	return response
}

func pluginRoot() string {
	return obsidian.PluginRoot()
}
