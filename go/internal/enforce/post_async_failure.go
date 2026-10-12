package enforce

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/lease"
)

// finalizeAsyncFailure adapts terminal async jobs to the shared synchronous
// failure finalizer. The caller owns the async claim lock and index.
func finalizeAsyncFailure(root string, event, index, pending map[string]any) (map[string]any, bool, error) {
	job := asyncJob(root, index)
	if job == nil {
		return nil, false, nil
	}
	prepFailure := event["error_code"] == "JOB_REPLAY_PREPARATION_FAILED"
	cancelRunning := event["hook_event_name"] == "PostToolUse" &&
		strings.HasSuffix(stringValue(event["tool_name"]), "__codex_cancel") && job["state"] == "RUNNING"
	switch job["state"] {
	case "FAILED", "TIMED_OUT", "CANCELLED", "EXPIRED":
	case "SUCCEEDED":
		if !prepFailure {
			return nil, false, nil
		}
	case "RUNNING":
		if !cancelRunning {
			return nil, false, nil
		}
	default:
		return nil, false, nil
	}
	if err := recordQuotaWakeFailure(root, pending, index, job); err != nil {
		return nil, false, err
	}
	project := stringValue(index["project_key"])
	if project == "" {
		project = stringValue(pending["project_key"])
	}
	owner := stringValue(pending["lease_owner"])
	if owner == "" {
		owner = stringValue(index["call_key"])
	}
	if project != "" && owner != "" {
		defer func() {
			_ = lease.New(filepath.Join(root, "projects", project)).Release(owner, float64(time.Now().UnixNano())/1e9)
		}()
	}
	key := stringValue(index["call_key"])
	if !asyncHash.MatchString(key) {
		return nil, true, errors.New("async failure call key is invalid")
	}
	if index["stage"] == "CONSUMED" {
		response, ok := index["response"].(map[string]any)
		if !ok || response == nil {
			return nil, true, errors.New("consumed async failure response is missing")
		}
		project := stringValue(index["project_key"])
		result := lease.New(filepath.Join(root, "projects", project)).Release(key, float64(time.Now().UnixNano())/1e9)
		if result.Status != "OK" && result.Code != "LEASE_OWNER_MISSING" {
			return nil, true, fmt.Errorf("scope lease cleanup failed: %s", result.Code)
		}
		pendingPath := filepath.Join(root, "pending", key+".json")
		if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, true, fmt.Errorf("pending cleanup failed: %w", err)
		}
		return response, true, nil
	}
	evidencePath := filepath.Join(root, "raw_tool_outputs", key+"-failure.json")
	if index["stage"] != "FINALIZING" {
		return nil, true, errors.New("async failure index is not finalizing")
	}
	failureEvent := make(map[string]any, len(event)+4)
	for name, value := range event {
		failureEvent[name] = value
	}
	failureEvent["tool_name"], failureEvent["session_id"], failureEvent["tool_use_id"] =
		"mcp__codex-main__codex", index["session_id"], pending["tool_use_id"]
	if duration := asyncFailureDuration(job); duration != nil {
		failureEvent["duration_ms"] = json.Number(strconv.FormatFloat(*duration, 'f', -1, 64))
	} else if duration, ok := failureEvent["duration_ms"].(float64); ok {
		failureEvent["duration_ms"] = json.Number(strconv.FormatFloat(duration, 'f', -1, 64))
	} else if duration, ok := failureEvent["duration_ms"].(int); ok {
		failureEvent["duration_ms"] = json.Number(strconv.Itoa(duration))
	}
	if failureEvent["error"] == nil {
		failureEvent["error"] = stringValue(job["errorCode"])
		if failureEvent["error"] == "" {
			failureEvent["error"] = "ASYNC_TERMINAL_FAILURE"
		}
	}
	if prepFailure {
		failureEvent["error_code"] = event["error_code"]
		failureEvent["replay_thread_id"] = event["replay_thread_id"]
	}
	if _, err := os.Stat(evidencePath); errors.Is(err, os.ErrNotExist) {
		evidence := failureEvidence(failureEvent, pending)
		if _, err := persistFailureEvidence(root, key, evidence); err != nil {
			failureObservation(root, failureEvent, pending)
			return failureErrorOutput(failureErrorText(err)), true, err
		}
	} else if evidence, err := readAsyncObject(evidencePath); err != nil || evidence["pending_call"] == nil {
		if err == nil {
			err = errors.New("async failure evidence is incomplete")
		}
		return failureErrorOutput(failureErrorText(err)), true, err
	} else {
		for _, name := range []string{"error", "is_interrupt", "duration_ms", "error_code", "replay_thread_id"} {
			if value, exists := evidence[name]; exists {
				failureEvent[name] = value
			}
		}
	}
	path, err := filepath.Abs(evidencePath)
	if err != nil {
		return failureErrorOutput(failureErrorText(err)), true, err
	}
	routingEnforced := pending["kind"] != "pretool_bypass"
	outcome := map[string]any{"status": "AMBIGUOUS_TOOL_FAILURE", "ambiguous_outcome": true,
		"work_performed": "UNKNOWN", "routing_enforced": routingEnforced,
		"evidence_path": path, "claude_review_mode": "FULL"}
	if err = saveFailureSession(root, failureEvent, pending, outcome); err != nil {
		failureObservation(root, failureEvent, pending)
		return failureErrorOutput(failureErrorText(err)), true, err
	}
	failureObservation(root, failureEvent, pending)
	response := failureSuccessOutput(path, routingEnforced)
	indexPath := filepath.Join(root, "async_jobs", stringValue(index["jobId"])+".json")
	index["terminal_failure"] = true
	if err = consumeAsyncResult(indexPath, index, path, response); err != nil {
		return failureErrorOutput(failureErrorText(err)), true, err
	}
	pendingPath := filepath.Join(root, "pending", key+".json")
	if err = cleanupFailureTail(root, pendingPath, failureEvent, pending, false); err != nil {
		return failureErrorOutput("WiringError: " + err.Error()), true, err
	}
	return response, true, nil
}

func recordQuotaWakeFailure(root string, pending, index, job map[string]any) error {
	if job["state"] != "FAILED" || stringValue(mapValue(job["launchFailureEvidence"])["type"]) != "CREDIT_EXHAUSTED" {
		return nil
	}
	repository := stringValue(pending["repo_root"])
	session := stringValue(index["session_id"])
	if session == "" {
		session = stringValue(pending["session_id"])
	}
	if repository == "" || session == "" {
		return nil
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		home = filepath.Join(userHome, ".codex")
	}
	now := time.Now().UTC()
	quota, ok := contextio.LatestQuota(home, now)
	if !ok {
		return nil
	}
	var reset time.Time
	for _, label := range []string{"5h", "7d"} {
		used, ok := quota[label+"_used_pct"].(float64)
		resetText, resetOK := quota[label+"_resets_at"].(string)
		at, err := time.Parse(time.RFC3339, resetText)
		if ok && used >= 100 && resetOK && err == nil && at.After(now) && at.After(reset) {
			reset = at
		}
	}
	if reset.IsZero() {
		return nil
	}
	return contextio.RecordQuotaWake(repository, root, session, stringValue(pending["task_id"]),
		reset.Format(time.RFC3339), "quota_failure", now)
}

func asyncFailureDuration(job map[string]any) *float64 {
	start, okStart := job["startedAt"].(float64)
	if !okStart {
		start, okStart = job["acceptedAt"].(float64)
	}
	finish, okFinish := job["finishedAt"].(float64)
	if !okStart || !okFinish || finish < start {
		return nil
	}
	duration := float64(int((finish - start) * 1000))
	return &duration
}
