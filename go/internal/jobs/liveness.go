package jobs

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
)

const (
	stallColdStartFloorSeconds   = 15 * 60
	stallColdStartCeilingSeconds = 30 * 60
	livenessCheckpointSeconds    = 60.0
)

// LivenessActivity uses the bridge's persisted field names. checkpointAt is
// deliberately process-local and is never written into the job record.
type LivenessActivity map[string]any

// RecordLivenessActivity measures the gap from the previous activity sample.
func RecordLivenessActivity(activity LivenessActivity, now float64) {
	if previous, ok := livenessNumber(activity["lastActivityAt"]); ok {
		longest, valid := livenessNumber(activity["longestSilentGapSeconds"])
		if !valid {
			longest = 0
		}
		activity["longestSilentGapSeconds"] = math.Max(longest, math.Max(0, now-previous))
	}
	activity["lastActivityAt"] = now
}

// NewLivenessActivity restores durable observations after a host restart while
// resetting process-local checkpoint timing, as the Python bridge does.
func NewLivenessActivity(job *Record, agent string, threshold int, now float64) LivenessActivity {
	var previous map[string]any
	job.Get("liveness", &previous)
	if previous == nil {
		previous = map[string]any{}
	}
	var route any
	job.Get("route", &route)
	lastActivity, ok := previous["lastActivityAt"]
	if !ok {
		lastActivity = now
	}
	stdoutBytes, ok := previous["stdoutBytes"]
	if !ok {
		stdoutBytes = 0
	}
	longest, ok := previous["longestSilentGapSeconds"]
	if !ok {
		longest = 0.0
	}
	activity := LivenessActivity{
		"route": route, "agent": agent, "lastActivityAt": lastActivity,
		"stdoutBytes": stdoutBytes, "processAlive": true,
		"processAliveAt": now, "longestSilentGapSeconds": longest,
		"stallThresholdSeconds": threshold, "checkpointAt": now,
	}
	var stalled any
	if job.Get("stallDetectedAt", &stalled) && stalled != nil {
		if _, valid := livenessNumber(stalled); valid {
			activity["stallDetectedAt"] = stalled
		}
	}
	return activity
}

// CheckpointLiveness persists a liveness snapshot only for the current running
// generation and owner. The bool reports whether a checkpoint was written.
func CheckpointLiveness(path string, generation int64, token string, activity LivenessActivity,
	now float64, processAlive, force bool) (bool, error) {
	silentFor := math.Max(0, now-numberOr(activity["lastActivityAt"], now))
	threshold, thresholdValid := livenessNumber(activity["stallThresholdSeconds"])
	shouldStall := thresholdValid && silentFor >= threshold
	_, stallValueValid := livenessNumber(activity["stallDetectedAt"])
	wasStalled := stallValueValid
	transition := shouldStall != wasStalled
	checkpointAt := numberOr(activity["checkpointAt"], 0)
	if !transition && !force && now-checkpointAt < livenessCheckpointSeconds {
		return false, nil
	}

	job, err := Read(path)
	if err != nil {
		return false, err
	}
	var state string
	job.Get("state", &state)
	if state != "RUNNING" {
		return false, nil
	}
	activity["processAlive"] = processAlive
	activity["processAliveAt"] = now
	activity["checkpointAt"] = now
	if shouldStall && !wasStalled {
		activity["stallDetectedAt"] = now
		if err = job.Set("stallDetectedAt", now); err != nil {
			return false, err
		}
		if err = job.Set("errorCode", "STALLED"); err != nil {
			return false, err
		}
	} else if !shouldStall && wasStalled {
		delete(activity, "stallDetectedAt")
		deleteRecordField(job, "stallDetectedAt")
		if code := rawString(job.values["errorCode"]); code == "STALLED" {
			deleteRecordField(job, "errorCode")
		}
	}
	if err = job.Set("activeAgent", activity["agent"]); err != nil {
		return false, err
	}
	persisted := make(map[string]any, len(activity)-1)
	for key, value := range activity {
		if key != "checkpointAt" {
			persisted[key] = value
		}
	}
	if err = job.Set("liveness", persisted); err != nil {
		return false, err
	}
	if err = WriteFenced(path, job, generation, token); err != nil {
		if errors.Is(err, ErrClaimConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func deleteRecordField(record *Record, key string) {
	if _, exists := record.values[key]; !exists {
		return
	}
	delete(record.values, key)
	for index, existing := range record.keys {
		if existing == key {
			record.keys = append(record.keys[:index], record.keys[index+1:]...)
			break
		}
	}
	record.dirty = true
}

// StallThreshold matches the bridge's route/agent history and cold-start rules.
func StallThreshold(jobDir string, job *Record) int {
	var route, agent string
	job.Get("route", &route)
	job.Get("activeAgent", &agent)
	var live map[string]any
	job.Get("liveness", &live)
	if agent == "" {
		agent, _ = live["agent"].(string)
	}
	samples := make([]float64, 0)
	if route != "" && agent != "" {
		entries, err := os.ReadDir(jobDir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
					continue
				}
				record, err := Read(filepath.Join(jobDir, entry.Name()))
				if err != nil {
					continue
				}
				var state, recordRoute, recordAgent string
				record.Get("state", &state)
				record.Get("route", &recordRoute)
				record.Get("activeAgent", &recordAgent)
				var recordLive map[string]any
				record.Get("liveness", &recordLive)
				if recordAgent == "" {
					recordAgent, _ = recordLive["agent"].(string)
				}
				gap, valid := livenessNumber(recordLive["longestSilentGapSeconds"])
				if state == "SUCCEEDED" && recordRoute == route && recordAgent == agent && valid && gap >= 0 {
					samples = append(samples, gap)
				}
			}
		}
	}
	if len(samples) >= 5 {
		sort.Float64s(samples)
		p90 := samples[int(math.Ceil(0.9*float64(len(samples))))-1]
		return max(stallColdStartFloorSeconds, int(math.Ceil(p90*1.5)))
	}
	if agent == "agy" {
		return stallColdStartCeilingSeconds
	}
	var eta any
	job.Get("etaMinutes", &eta)
	seconds, valid := livenessNumber(eta)
	if !valid || seconds <= 0 {
		seconds = stallColdStartFloorSeconds
	} else {
		seconds *= 60
	}
	return max(stallColdStartFloorSeconds, min(stallColdStartCeilingSeconds, int(math.Ceil(seconds*2))))
}

func livenessNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, !math.IsNaN(number) && !math.IsInf(number, 0)
	default:
		return 0, false
	}
}

func numberOr(value any, fallback float64) float64 {
	if number, ok := livenessNumber(value); ok {
		return number
	}
	return fallback
}

func rawString(value []byte) string {
	var result string
	_ = json.Unmarshal(value, &result)
	return result
}
