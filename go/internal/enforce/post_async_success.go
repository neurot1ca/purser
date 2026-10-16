package enforce

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/ledger"
	"purser-hook/internal/pluginroot"
	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

type asyncReplayPreparationError struct {
	status string
	err    error
}

func (e *asyncReplayPreparationError) Error() string {
	return "worker replay preparation failed (status=" + e.status + "): " + e.err.Error()
}

func (e *asyncReplayPreparationError) Unwrap() error { return e.err }

// WorktreeRetirementDispatcher is installed by jobs to keep enforce independent
// of the package that owns retirement dispatch.
var WorktreeRetirementDispatcher func(stateRoot, jobID string) error

// finishAsyncSuccess assembles the already-claimed codex_result success path.
// Inputs that belong to the surrounding Post lane are explicit so this remains
// unwired until that lane is migrated.
func finishAsyncSuccess(root string, event, index, pending, original map[string]any,
	repoRoot string, loaded map[string]any, resolved postProfile, checks []profile.Check,
	gateConfig map[string]any, cap, reportLimit int, hookBudget time.Duration) (map[string]any, error) {
	job := asyncJob(root, index)
	value := firstCodexResult(event["tool_response"])
	thread, content := stringValue(value["threadId"]), stringValue(value["content"])
	if job == nil || job["state"] != "SUCCEEDED" || thread == "" || thread != job["threadId"] ||
		value["jobId"] != index["jobId"] || value["state"] != "SUCCEEDED" || codexResponseIsError(event["tool_response"]) {
		return nil, errors.New("async result does not match successful durable job")
	}
	if gap, ok := job["liveness"].(map[string]any); ok {
		if seconds, ok := gap["longestSilentGapSeconds"].(float64); ok && seconds >= 0 {
			pending["longest_silent_gap_seconds"] = seconds
		}
	}
	worktree := stringValue(value["worktreePath"])
	if worktree == "" {
		worktree = stringValue(job["worktreePath"])
	}
	if worktree != "" {
		pending["worktree_path"], pending["worktree_snapshot"] = worktree, job["snapshot"]
		if err := writeAsyncObject(pendingPathForAsync(root, original), pending); err != nil {
			return nil, err
		}
	}
	if agent := stringValue(value["servedAgent"]); agent == "codex" || agent == "opencode" || agent == "agy" {
		if model := stringValue(value["servedModel"]); model != "" {
			pending["agent"], pending["model"], pending["attempted_models"] = agent, model, value["attemptedModels"]
			if err := writeAsyncObject(pendingPathForAsync(root, original), pending); err != nil {
				return nil, err
			}
		}
	}
	bundle := baseBundle("")
	bundle["task_id"], bundle["phase"], bundle["route"] = pending["task_id"], pending["phase"], pending["route"]
	if bindingError := stringValue(job["planBindingError"]); bindingError != "" {
		bundle["force_full_review"] = true
		bundle["findings"] = appendPostFinding(bundle["findings"], "PLAN_BINDING_INTERNAL_ERROR", "Plan binding failed internally during launch: "+bindingError)
	}
	implementer := stringValue(pending["agent"])
	if implementer == "" {
		implementer = "codex"
	}
	bundle["implementer"] = implementer + "-main"
	bundle["model"], bundle["effort"], bundle["thread_id"] = pending["model"], pending["effort"], thread
	bundle["reviewer1_model_requested"] = loaded["reviewer1_model_requested"]
	bundle["implementer_report"], bundle["raw_output_path"] = content, ""
	bundle["routing_enforced"], bundle["task_contract_grounded"] = pending["kind"] != "pretool_bypass", pending["task_contract_grounded"] == true
	bundle["worker_validation"] = parseWorkerValidation(content)
	if worker := mapValue(pending["worker_validation"]); worker["status"] != nil {
		bundle["worker_validation"] = worker
		if worker["source"] == "HOST_TEST_EVIDENCE" {
			bundle["validation"] = worker["status"]
		}
	}
	evidence, _ := job["testEvidence"].([]any)
	if len(evidence) > 0 {
		worker := mapValue(bundle["worker_validation"])
		worker["test_evidence"] = evidence
		worker["source"] = "HOST_TEST_EVIDENCE"
		status := "PASS"
		malformed := false
		failed := job["testEvidenceFailed"] == true
		latest := make(map[string]map[string]any, len(evidence))
		for _, raw := range evidence {
			entry := mapValue(raw)
			latest[stringValue(entry["command"])] = entry
		}
		for _, entry := range latest {
			value := mapValue(entry)["exit_code"]
			code, ok := value.(float64)
			if !ok {
				if integer, valid := value.(int); valid {
					code, ok = float64(integer), true
				}
			}
			if !ok || code != float64(int(code)) {
				malformed = true
				continue
			}
			if code != 0 {
				failed = true
			}
		}
		if failed {
			status = "FAIL"
		} else if malformed {
			status = "UNKNOWN"
		}
		worker["status"] = status
		bundle["validation"] = status
	}
	if stringValue(pending["phase"]) == "plan" {
		if _, planErr := storePostPlan(root, original, pending["task_id"], content, bundle); planErr != nil {
			return nil, planErr
		}
	}
	normalized := pythonCodexResultJSON(event["tool_response"], value)
	if len(normalized) == 0 {
		normalized, _ = canonicalJSON(value)
	}
	toolResponse := map[string]any{"content": []any{map[string]any{"type": "text", "text": string(normalized)}}}
	rawPath, err := persistRawOutput(root, original, toolResponse)
	if err != nil {
		return nil, err
	}
	bundle["raw_output_path"] = rawPath
	if worktree != "" && job["applyState"] != "applied" {
		recoveryPath := filepath.Join(root, "preparations", stringValue(index["jobId"]))
		if revision := job["snapshotRevision"]; revision != nil && revision != "" && revision != 0 {
			recoveryPath = filepath.Join(recoveryPath, fmt.Sprintf("revision-%v", revision))
		}
		prepared, prepErr := ledger.PrepareJobReplay(stringValue(job["repoRoot"]), worktree, stringValue(job["snapshot"]),
			recoveryPath)
		if prepErr != nil {
			return nil, &asyncReplayPreparationError{status: "error", err: prepErr}
		}
		if prepared["status"] == "conflict" || prepared["status"] != "prepared" && prepared["status"] != "unchanged" {
			return nil, fmt.Errorf("worker replay preparation returned status=%s", stringValue(prepared["status"]))
		}
		if prepared["status"] == "prepared" {
			job["snapshot"] = prepared["snapshot"]
			if err = writeAsyncObject(filepath.Join(root, "codex_exec_mcp", "jobs", stringValue(index["jobId"])+".json"), job); err != nil {
				return nil, err
			}
			pending["worktree_snapshot"] = prepared["snapshot"]
			if err = writeAsyncObject(pendingPathForAsync(root, original), pending); err != nil {
				return nil, err
			}
		}
	}
	if repoRoot == "" {
		repoRoot = stringValue(pending["repo_root"])
	}
	if _, err = assemblePostDelta(repoRoot, pending, bundle, resolved.ProtectedPaths, nil); err != nil {
		return nil, err
	}
	noDelta := stringValue(pending["phase"]) == "operation"
	zeroDelta := false
	if count, exists := bundle["delta_files"]; exists && numberOr(count, -1) == 0 {
		noDelta = true
		zeroDelta = true
	}
	paths, pathsOK := postStringList(bundle["changed_paths"])
	if mapValue(bundle["delta"])["status"] == ledger.OK && pathsOK && len(paths) == 0 {
		noDelta = true
		zeroDelta = true
	}
	if noDelta {
		owner := stringValue(pending["lease_owner"])
		if owner != "" {
			err = leaseRelease(root, stringValue(pending["project_key"]), owner)
			if err != nil && err.Error() != "LEASE_OWNER_MISSING" {
				return nil, fmt.Errorf("scope lease cleanup failed: %w", err)
			}
		}
	}
	schedulePostReview(repoRoot, pending, bundle, resolved, checks, gateConfig, 30*time.Second, true, true)
	delta := mapValue(bundle["delta"])
	if bundle["reviewer1_skipped_reason"] == nil && bundle["claude_review_mode"] == review.Full {
		pluginRoot := pluginroot.Root()
		if pluginRoot == "" {
			pluginRoot = repoRoot
		}
		reviewerStarted := time.Now()
		result, runErr := runPostReviewer(pluginRoot, root, pending, bundle, resolved, loaded, delta, mapValue(bundle["concurrent_writer"]), time.Now(), hookBudget)
		started := bundle["reviewer1_skipped_reason"] == nil
		recordPostReviewerObservations(root, original, &repoRoot, stringValue(pending["task_id"]), pendingProjectKey(pending),
			started, stringValue(result.Attributes["provider"]), stringValue(result.Attributes["reviewer1_model_requested"]), stringValue(bundle["reviewer1_skipped_reason"]),
			result, runErr, time.Since(reviewerStarted), -1)
		if runErr != nil {
			bundle["claude_review_mode"] = review.Full
			bundle["findings"] = appendPostFinding(bundle["findings"], "INTERNAL_POST_ERROR", "Reviewer failed during async result review.")
		} else {
			attrs := result.Attributes
			for _, key := range []string{"reviewer1_provider", "reviewer1_model_requested", "reviewer1_scope"} {
				if v, ok := attrs[key]; ok {
					bundle[key] = v
				}
			}
			if validation, ok := attrs["validation"]; ok {
				bundle["validation"] = postReviewerValidation(bundle, stringValue(validation))
			}
			bundle["reviewer1_result"] = attrs["result"]
			for _, finding := range postReviewerFindings(attrs["findings"]) {
				bundle["findings"] = appendPostFinding(bundle["findings"], stringValue(finding["code"]), stringValue(finding["message"]), stringValue(finding["reference"]))
			}
			mode := review.ReviewMode(true, routeInList(gateConfig["always_full_routes"], stringValue(pending["route"])), "", stringValue(attrs["result"]))
			if resolved.ForceFullReview || bundle["force_full_review"] == true || pending["task_contract_grounded"] != true {
				mode.Mode = review.Full
			}
			bundle["claude_review_mode"] = mode.Mode
		}
	} else if reason := stringValue(bundle["reviewer1_skipped_reason"]); reason != "" {
		recordPostReviewerObservations(root, original, &repoRoot, stringValue(pending["task_id"]), pendingProjectKey(pending),
			false, "", stringValue(loaded["reviewer1_model_requested"]), reason, review.BridgeOutcome{}, nil, 0, -1)
	}
	job["reviewedOutcome"] = bundle["reviewer1_result"]
	job["reviewFindingRefs"] = []any{}
	for _, finding := range postReviewerFindings(bundle["findings"]) {
		if ref := stringValue(finding["reference"]); ref != "" {
			job["reviewFindingRefs"] = append(job["reviewFindingRefs"].([]any), ref)
		}
	}
	if worktree != "" {
		job["reviewedSnapshot"] = job["snapshot"]
		delete(job, "reviewRefreshPending")
	}
	if err = writeAsyncObject(filepath.Join(root, "codex_exec_mcp", "jobs", stringValue(index["jobId"])+".json"), job); err != nil {
		return nil, err
	}
	if taskID := stringValue(pending["task_id"]); taskID != "" {
		if err = SaveTaskAttempt(root, TaskAttempt{ProjectKey: pendingProjectKey(pending), TaskID: taskID,
			TerminalDisposition: stringValue(bundle["reviewer1_result"])}); err != nil {
			log.Printf("task attempt persistence failed for result job %s: %v", stringValue(index["jobId"]), err)
		}
	}
	finishedAt := float64(time.Now().UnixNano()) / 1e9
	if value, ok := job["finishedAt"].(float64); ok && value > 0 {
		finishedAt = value
	}
	if pendingAgent(pending) == "codex" {
		bundle["codex_quota"] = postReadQuota(thread)
	}
	if err = finishPostSession(root, original, pending, bundle, thread, repoRoot, worktree != "" && job["applyState"] != "applied", finishedAt, numberOr(loaded["manifest_budget_seconds"], 3)); err != nil {
		return nil, err
	}
	bundle["audit_receipt"] = postAsyncAuditReceipt(bundle)
	compact, artifact, err := finalizePostArtifacts(root, original, bundle, cap, reportLimit)
	if err != nil {
		return nil, err
	}
	response, err := replacement(compact, stringValue(loaded["post_tool_output_field"]))
	if err != nil {
		return nil, err
	}
	appendPostQuotaSystemMessage(response, bundle["codex_quota"])
	postGearAdvice(root, original, pending, loaded, response,
		workerDuration(job["startedAt"], job["finishedAt"]), stringValue(index["jobId"]))
	if bundle["apply"] == "codex_apply" {
		appendPostApplyGuidance(response, stringValue(index["jobId"]))
	}
	recordReviewIntervalDelivery(root, event, stringValue(index["jobId"]), stringValue(pending["task_id"]), bundle)
	index["terminal_failure"] = false
	if err = consumeAsyncResult(filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json"), index, artifact, response); err != nil {
		return nil, err
	}
	if zeroDelta && WorktreeRetirementDispatcher != nil {
		if err = WorktreeRetirementDispatcher(root, stringValue(index["jobId"])); err != nil {
			diagnosticErr := writePostDiagnostic(root, event, fmt.Errorf("worktree retirement dispatch failed: %w", err))
			if diagnosticErr != nil {
				log.Printf("worktree retirement dispatch failed for job %s: %v; diagnostic write failed: %v", index["jobId"], err, diagnosticErr)
			}
		}
	}
	return response, nil
}

// pythonCodexResultJSON preserves the result object's key order and Python's
// default separator spacing when storing the normalized async tool response.
func pythonCodexResultJSON(response, expected any) []byte {
	envelope, ok := response.(map[string]any)
	if !ok {
		return nil
	}
	items, _ := envelope["content"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "text" {
			continue
		}
		text := stringValue(item["text"])
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(text)) != nil {
			return nil
		}
		input := compact.Bytes()
		var parsed any
		if json.Unmarshal(input, &parsed) != nil {
			return nil
		}
		actual, _ := canonicalJSON(parsed)
		wanted, _ := canonicalJSON(expected)
		if !bytes.Equal(actual, wanted) {
			return nil
		}
		output := make([]byte, 0, len(input)+len(input)/8)
		inString, escaped := false, false
		for i, ch := range input {
			output = append(output, ch)
			if inString {
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					inString = false
				}
				continue
			}
			if ch == '"' {
				inString = true
			} else if (ch == ',' || ch == ':') && i+1 < len(input) && input[i+1] != ' ' {
				output = append(output, ' ')
			}
		}
		return output
	}
	return nil
}

func postAsyncAuditReceipt(bundle map[string]any) map[string]any {
	paths, ok := postStringList(bundle["changed_paths"])
	if bundle["claude_review_mode"] != review.Audit || !ok || len(paths) == 0 {
		return nil
	}
	if len(paths) > 10 {
		paths = paths[:10]
	}
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = fmt.Sprintf("%q", path)
	}
	task := stringValue(bundle["task_id"])
	if task == "" {
		task = "task"
	}
	return map[string]any{"verdict": "READY_FOR_ONE_TURN_ACCEPT", "implementer": bundle["implementer"],
		"model": bundle["model"], "effort": bundle["effort"],
		"reviewer1_verdict":  firstNonNil(bundle["reviewer1_result"], bundle["reviewer1_skipped_reason"]),
		"validation_summary": fmt.Sprintf("validation=%v, delta=%vL/%vF", bundle["validation"], bundle["delta_lines"], bundle["delta_files"]),
		"commit_command":     fmt.Sprintf("git add %s && git commit -m %q", strings.Join(quoted, " "), "feat("+task+"): implement changes")}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstCodexResult(value any) map[string]any {
	objects := decodedCodexObjects(value)
	if len(objects) == 0 {
		return nil
	}
	return objects[0]
}

func pendingPathForAsync(root string, original map[string]any) string {
	key, _ := callKey(original)
	return filepath.Join(root, "pending", key+".json")
}

func writeAsyncObject(path string, value map[string]any) error {
	return atomicAdmissionJSON(path, value)
}
