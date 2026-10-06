package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const gearUpMaster = "Codex quota has room before it resets. Switch luna-high work to sol-medium to finish faster? Run /purser:gear-up or just tell me."
const gearDownMaster = "Codex quota is running low before it resets. Switch back to luna-high to save quota? Run /purser:gear-down or just tell me."

// QueueGearAdviceRecovery preserves a claimed suggestion when Post context
// could not be attached. SessionStart revalidates it before delivery.
func QueueGearAdviceRecovery(root, projectKey string, input GearAdviceInput, advice GearAdvice, source, window string) error {
	if root == "" || projectKey == "" || input.Route == "" || source == "" || window == "" || advice != GearAdviceUp && advice != GearAdviceDown {
		return os.ErrInvalid
	}
	key := sha256.Sum256([]byte(source + "\x00" + window))
	path := filepath.Join(root, "gear-advice", "pending", hex.EncodeToString(key[:])+".json")
	record := map[string]any{"advice": string(advice), "source_identity": source, "window_id": window,
		"project_key": projectKey,
		"route":       input.Route, "eligible": input.Eligible, "duration_ms": input.DurationMS,
		"implementer": input.Implementer, "completed_job": input.CompletedJob}
	return writeJSONAtomically(path, record)
}

// ClearGearAdviceRecovery removes the pending delivery for one shared claim.
func ClearGearAdviceRecovery(root, source, window string) {
	if root == "" || source == "" || window == "" {
		return
	}
	key := sha256.Sum256([]byte(source + "\x00" + window))
	_ = os.Remove(filepath.Join(root, "gear-advice", "pending", hex.EncodeToString(key[:])+".json"))
}

// ClearCompletedGearAdviceRecovery clears durable advice once its Post result
// has been recorded for replay.
func ClearCompletedGearAdviceRecovery(root, projectKey, jobID string) {
	if root == "" || projectKey == "" || jobID == "" {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(root, "gear-advice", "pending", "*.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		var record map[string]any
		if err == nil && json.Unmarshal(data, &record) == nil && record["project_key"] == projectKey && record["completed_job"] == jobID {
			_ = os.Remove(path)
		}
	}
}

func recoverGearAdvice(root, projectKey string, now time.Time) string {
	paths, _ := filepath.Glob(filepath.Join(root, "gear-advice", "pending", "*.json"))
	if len(paths) == 0 {
		return ""
	}
	quotaHome := os.Getenv("CODEX_HOME")
	if quotaHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		quotaHome = filepath.Join(home, ".codex")
	}
	quota, ok := LatestQuota(quotaHome, now)
	if !ok {
		return ""
	}
	gearUp, err := Gear(root, projectKey)
	if err != nil {
		return ""
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		var record map[string]any
		if readErr != nil || json.Unmarshal(data, &record) != nil {
			continue
		}
		if record["project_key"] != projectKey {
			continue
		}
		advice := GearAdvice(str(record["advice"]))
		duration, _ := numeric(record["duration_ms"])
		input := GearAdviceInput{GearUp: gearUp, Quota: quota, Now: now, Route: str(record["route"]),
			Eligible: record["eligible"] == true, DurationMS: duration,
			Implementer: str(record["implementer"]), CompletedJob: str(record["completed_job"]),
			Samples: RouteDurationSamples(root, projectKey, str(record["route"]))}
		result := EvaluateGearAdvice(input)
		if result.Advice != advice || result.WindowID != record["window_id"] ||
			quota["source_identity"] != record["source_identity"] {
			_ = os.Remove(path)
			continue
		}
		_ = os.Remove(path)
		return gearAdviceInstruction(advice)
	}
	return ""
}

func gearAdviceInstruction(advice GearAdvice) string {
	master := gearUpMaster
	if advice == GearAdviceDown {
		master = gearDownMaster
	}
	return "On your next reply, render exactly one line in the user's language, translating from the English text and keeping model names and commands unchanged:\n" + master
}

// GearAdviceInstruction formats the approved master with translation guidance.
func GearAdviceInstruction(advice GearAdvice) string { return gearAdviceInstruction(advice) }
