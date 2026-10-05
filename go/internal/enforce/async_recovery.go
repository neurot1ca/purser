package enforce

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

// recoverAsyncFinalizing reuses durable Post state after a worker died between
// writing its review artifact and consuming the async job index. handled=false
// is the Python fallback: continue through ordinary result finalization.
func recoverAsyncFinalizing(root string, event, index, pending map[string]any, cap, reportLimit int) (response map[string]any, handled bool, err error) {
	if index["stage"] == "CONSUMED" {
		stored, ok := index["response"].(map[string]any)
		if !ok || stored == nil {
			return nil, false, nil
		}
		return stored, true, nil
	}
	if index["stage"] != "FINALIZING" {
		return nil, false, nil
	}
	key := stringValue(index["call_key"])
	if key == "" {
		return nil, false, errors.New("async finalizing call key is missing")
	}
	artifact := filepath.Join(root, "bundle_artifacts", key+".json")
	raw := filepath.Join(root, "raw_tool_outputs", key+".json")
	if !isFile(artifact) || !isFile(raw) {
		return nil, false, nil
	}
	bundle, err := readAsyncObject(artifact)
	if err != nil {
		return nil, false, fmt.Errorf("async review artifact is invalid: %w", err)
	}
	absoluteRaw, err := filepath.Abs(raw)
	if err != nil {
		return nil, false, err
	}
	if bundle["task_id"] != pending["task_id"] || bundle["raw_output_path"] != absoluteRaw {
		return nil, false, nil
	}
	if finding := asyncRecoveryLedgerRejection(root, index, pending, artifact, absoluteRaw); finding != nil {
		findings, ok := bundle["findings"].([]any)
		if !ok {
			findings = []any{}
		}
		found := false
		for _, item := range findings {
			if row, ok := item.(map[string]any); ok && row["code"] == "LEDGER_RECORD_REJECTED" {
				found = true
				break
			}
		}
		if !found {
			bundle["findings"] = append(findings, finding)
		}
		bundle["claude_review_mode"], bundle["audit_receipt"] = "FULL", nil
	}
	job := asyncJob(root, index)
	keepWorktree := job != nil && stringValue(job["worktreePath"]) != "" && job["applyState"] != "applied"
	compact, path, err := finalizePostArtifacts(root, event, bundle, cap, reportLimit)
	if err != nil {
		return nil, false, err
	}
	response, err = replacement(compact, "updatedToolOutput")
	if err != nil {
		return nil, false, err
	}
	if bundle["apply"] == "codex_apply" && job != nil {
		response["systemMessage"] = PostApplyGuidance(stringValue(response["systemMessage"]), stringValue(index["jobId"]))
	}
	index["stage"], index["bundle_path"], index["response"] = "CONSUMED", path, response
	encoded, err := canonicalJSON(index)
	if err != nil {
		return nil, false, err
	}
	indexPathID := stringValue(index["jobId"])
	if indexPathID == "" {
		indexPathID = key
	}
	if err = atomicWrite(filepath.Join(root, "async_jobs", indexPathID+".json"), encoded); err != nil {
		return nil, false, err
	}
	if !keepWorktree {
		owner := stringValue(pending["lease_owner"])
		if owner != "" {
			project := asyncPendingProjectKey(pending)
			if project == "" {
				project = stringValue(index["project_key"])
			}
			result := lease.New(asyncProjectStateRoot(root, project)).Release(owner, float64(time.Now().UnixNano())/1e9)
			if result.Status != ledger.OK {
				return nil, false, fmt.Errorf("scope lease cleanup failed: %s", result.Code)
			}
		}
		for _, pendingFile := range pendingCandidates(root, event) {
			if removeErr := os.Remove(pendingFile); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return nil, false, fmt.Errorf("pending cleanup failed: %w", removeErr)
			}
		}
	}
	return response, true, nil
}

func asyncRecoveryLedgerRejection(root string, index, pending map[string]any, artifact, raw string) map[string]any {
	project := stringValue(index["project_key"])
	task := stringValue(pending["task_id"])
	path, err := ledger.PathForTask(asyncProjectStateRoot(root, project), task)
	loaded := ledger.Result{Status: ledger.CORRUPT}
	if err == nil {
		loaded = ledger.Load(path, task)
	}
	last, _ := loaded.Record["last_outcome"].(map[string]any)
	if loaded.Status == ledger.OK && last["raw_output_path"] == raw {
		return nil
	}
	detail := "The session ledger did not retain this job's thread and outcome."
	if bundle, readErr := readAsyncObject(artifact); readErr == nil {
		if findings, ok := bundle["findings"].([]any); ok {
			for _, item := range findings {
				finding, ok := item.(map[string]any)
				if ok && len(stringValue(finding["code"])) >= 7 && stringValue(finding["code"])[:7] == "LEDGER_" {
					detail = fmt.Sprintf("%s: %s", finding["code"], stringValue(finding["message"]))
					break
				}
			}
		}
	}
	return map[string]any{"code": "LEDGER_RECORD_REJECTED", "message": detail}
}

func asyncPendingProjectKey(pending map[string]any) string {
	if key := stringValue(pending["project_key"]); key != "" {
		return key
	}
	repository := stringValue(pending["repo_root"])
	if repository == "" {
		return "unresolved"
	}
	return projectKey(&repository)
}

func asyncProjectStateRoot(root, project string) string {
	if project == "" {
		project = "unresolved"
	}
	return filepath.Join(root, "projects", project)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
