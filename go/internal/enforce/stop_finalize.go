package enforce

import (
	"encoding/json"
	"os"
	"time"
)

const stopFinalizeStaleAfter = 24 * time.Hour

// finalizeStopTaskDecision ports the locked decision prefix of
// finalize_stop_task. Transcript checks, aggregation, writes, and observations
// remain in later migration passes.
func finalizeStopTaskDecision(root, session string, timeout time.Duration, now time.Time) map[string]any {
	if session == "" {
		return nil
	}
	path := activeTaskPath(root, session)
	markerPath := taskUsageContentionPath(root, session)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if _, markerErr := os.Stat(markerPath); os.IsNotExist(markerErr) {
			return nil
		}
	}

	lock, acquired := acquireTaskUsageLock(root, session, timeout)
	if !acquired {
		markTaskUsageContention(root, session, "")
		return map[string]any{"status": "FAILED", "reason": "ACTIVE_TASK_STATE_CONTENTION"}
	}
	defer releaseTaskUsageLock(lock)
	return finalizeStopTaskDecisionLocked(root, session, path, markerPath, now)
}

// finalizeStopTaskDecisionLocked performs the decision while the caller holds
// the per-session lock. Stop finalization keeps this lock through aggregation
// and persistence as well.
func finalizeStopTaskDecisionLocked(root, session, path, markerPath string, now time.Time) map[string]any {
	markerPresent, _ := readTaskUsageContention(root, session)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if markerPresent {
			removeTaskUsageContention(root, session)
		}
		return nil
	}
	if err != nil {
		if markerPresent {
			removeTaskUsageContention(root, session)
		}
		return nil
	}
	var active map[string]any
	if json.Unmarshal(data, &active) != nil || active == nil {
		if markerPresent {
			removeTaskUsageContention(root, session)
		}
		return nil
	}

	if active["completed"] == true {
		if markerPresent {
			removeTaskUsageContention(root, session)
		}
		if active["invalidated"] == true {
			reason, ok := active["invalidated_reason"]
			if !ok {
				reason = "OVERLAPPING_TASK_BOUNDARY"
			}
			return map[string]any{"status": "FAILED", "reason": reason}
		}
		return nil
	}
	if active["invalidated"] == true {
		if markerPresent {
			removeTaskUsageContention(root, session)
		}
		reason, ok := active["invalidated_reason"]
		if !ok {
			reason = "OVERLAPPING_TASK_BOUNDARY"
		}
		return map[string]any{"status": "FAILED", "reason": reason}
	}
	if markerPresent {
		return map[string]any{"status": "FAILED", "reason": "ACTIVE_TASK_STATE_CONTENTION"}
	}

	taskID, taskOK := active["task_id"].(string)
	toolUseID, toolOK := active["tool_use_id"].(string)
	if !taskOK || taskID == "" || !toolOK || toolUseID == "" {
		return nil
	}
	if created, ok := active["created_at"].(string); ok && created != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, created)
		if parseErr != nil || now.Sub(parsed) > stopFinalizeStaleAfter {
			return map[string]any{"status": "FAILED", "reason": "STALE_ACTIVE_TASK"}
		}
	}
	return map[string]any{"status": "READY"}
}
