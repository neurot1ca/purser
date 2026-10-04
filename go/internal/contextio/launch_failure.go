package contextio

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
)

func lastLaunchFailure(root, projectKey string) map[string]any {
	indexes := filepath.Join(root, "async_jobs")
	jobs := filepath.Join(root, "codex_exec_mcp", "jobs")
	var latest map[string]any
	var latestID string
	var latestAt float64
	found := false
	entries, err := os.ReadDir(indexes)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		indexData, err := os.ReadFile(filepath.Join(indexes, entry.Name()))
		if err != nil {
			continue
		}
		var index map[string]any
		if json.Unmarshal(indexData, &index) != nil || index["project_key"] != projectKey {
			continue
		}
		jobID, ok := index["jobId"].(string)
		if !ok || !validLaunchJobID(jobID) {
			continue
		}
		jobData, err := os.ReadFile(filepath.Join(jobs, jobID+".json"))
		if err != nil {
			continue
		}
		var job map[string]any
		if json.Unmarshal(jobData, &job) != nil || job["jobId"] != jobID {
			continue
		}
		phase, exists := job["phase"]
		if !exists {
			phase = job["jobPhase"]
		}
		if phase != "implementation" {
			continue
		}
		stamp, ok := job["finishedAt"].(float64)
		if !ok || math.IsNaN(stamp) || math.IsInf(stamp, 0) {
			continue
		}
		if !found || stamp > latestAt {
			latest, latestID, latestAt, found = job, jobID, stamp, true
		}
	}
	if !found || (latest["state"] != "FAILED" && latest["state"] != "TIMED_OUT") {
		return nil
	}
	evidence, ok := latest["launchFailureEvidence"].(map[string]any)
	if !ok {
		return nil
	}
	kind, ok := evidence["type"].(string)
	if !ok || (kind != "CREDIT_EXHAUSTED" && kind != "RATE_LIMITED" && kind != "NO_RESPONSE") {
		return nil
	}
	result := make(map[string]any, len(evidence)+2)
	for key, value := range evidence {
		result[key] = value
	}
	result["job_id"], result["trigger_id"] = latestID, latestID
	return result
}

// LastLaunchFailure exposes the latest typed implementation launch failure to Pre.
func LastLaunchFailure(root, projectKey string) map[string]any {
	return lastLaunchFailure(root, projectKey)
}

func validLaunchJobID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
