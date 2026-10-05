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

	"purser-hook/internal/ledger"
	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

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
		bundle["validation"] = worker["status"]
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
	if worktree != "" {
		recoveryPath := filepath.Join(root, "preparations", stringValue(index["jobId"]))
		if revision := job["snapshotRevision"]; revision != nil && revision != "" && revision != 0 {
			recoveryPath = filepath.Join(recoveryPath, fmt.Sprintf("revision-%v", revision))
		}
		prepared, prepErr := ledger.PrepareJobReplay(stringValue(job["repoRoot"]), worktree, stringValue(job["snapshot"]),
			recoveryPath)
		if prepErr != nil || prepared["status"] == "conflict" || prepared["status"] != "prepared" && prepared["status"] != "unchanged" {
			return nil, fmt.Errorf("worker replay preparation failed")
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
	schedulePostReview(repoRoot, pending, bundle, resolved, checks, gateConfig, 30*time.Second, true, true)
	delta := mapValue(bundle["delta"])
	if bundle["reviewer1_skipped_reason"] == nil && bundle["claude_review_mode"] == review.Full {
		pluginRoot := os.Getenv("CLAUDE_PLUGIN_ROOT")
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
			bundle["findings"] = appendPostFinding(bundle["findings"], "INTERNAL_POST_ERROR", "Reviewer-1 failed during async result review.")
		} else {
			attrs := result.Attributes
			for _, key := range []string{"reviewer1_provider", "reviewer1_model_requested", "reviewer1_scope", "validation"} {
				if v, ok := attrs[key]; ok {
					bundle[key] = v
				}
			}
			bundle["reviewer1_result"] = attrs["result"]
			if findings, ok := attrs["findings"].([]any); ok && len(findings) > 0 {
				current, ok := bundle["findings"].([]any)
				if !ok {
					current = []any{}
				}
				bundle["findings"] = append(current, findings...)
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
	if worktree != "" {
		job["reviewedSnapshot"] = job["snapshot"]
		delete(job, "reviewRefreshPending")
		if err = writeAsyncObject(filepath.Join(root, "codex_exec_mcp", "jobs", stringValue(index["jobId"])+".json"), job); err != nil {
			return nil, err
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
	if bundle["apply"] == "codex_apply" {
		response["systemMessage"] = PostApplyGuidance(stringValue(response["systemMessage"]), stringValue(index["jobId"]))
	}
	if err = consumeAsyncResult(filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json"), index, artifact, response); err != nil {
		return nil, err
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
