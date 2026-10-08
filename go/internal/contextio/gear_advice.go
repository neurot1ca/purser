package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"time"
)

type GearAdvice string

const (
	GearAdviceNone  GearAdvice = "none"
	GearAdviceUp    GearAdvice = "suggest_up"
	GearAdviceDown  GearAdvice = "suggest_down"
	gearQuotaMaxAge            = 30 * time.Minute
)

type GearAdviceInput struct {
	GearUp       bool
	Quota        map[string]any
	Now          time.Time
	Route        string
	Eligible     bool
	DurationMS   float64
	Samples      []DurationSample
	Implementer  string
	CompletedJob string
}

type GearAdviceResult struct {
	Advice   GearAdvice
	WindowID string
}

// EvaluateGearAdvice only evaluates evidence. ClaimGearAdvice persists the
// cross-session throttle separately so callers can keep delivery out of M2.
func EvaluateGearAdvice(input GearAdviceInput) GearAdviceResult {
	none := GearAdviceResult{Advice: GearAdviceNone}
	if input.Now.IsZero() || input.Route == "" || input.Quota == nil {
		return none
	}
	quota := input.Quota
	if quota["windows_seen"] != true {
		return none
	}
	snapshotAt, ok := quota["snapshot_at"].(string)
	if !ok {
		return none
	}
	stamp, err := time.Parse(time.RFC3339Nano, snapshotAt)
	if err != nil || stamp.After(input.Now) || input.Now.Sub(stamp) > gearQuotaMaxAge {
		return none
	}
	fiveHour, fiveState := parseGearQuotaWindow(quota, "5h", input.Now)
	week, weekState := parseGearQuotaWindow(quota, "7d", input.Now)
	if fiveState == quotaInvalid || weekState == quotaInvalid || weekState != quotaValid {
		return none
	}
	weeklyOnly := false
	if fiveState == quotaAbsent {
		plan, _ := quota["plan_type"].(string)
		if plan != "weekly-only" && plan != "weekly_only" && plan != "weekly" {
			return none
		}
		weeklyOnly = true
	}
	source, _ := quota["source_identity"].(string)
	if source == "" {
		return none
	}
	window := fiveHour
	windowName := "5h"
	if weeklyOnly {
		window = week
		windowName = "7d"
	}
	windowID := windowName + ":" + window.reset.UTC().Format(time.RFC3339Nano)
	if input.GearUp {
		if fiveState == quotaValid && fiveHour.used >= 85 && fiveHour.reset.Sub(input.Now) > time.Hour ||
			week.used >= 90 && week.reset.Sub(input.Now) > 24*time.Hour {
			return GearAdviceResult{Advice: GearAdviceDown, WindowID: windowID}
		}
		return none
	}
	if !input.Eligible || input.DurationMS <= 0 || math.IsNaN(input.DurationMS) || math.IsInf(input.DurationMS, 0) {
		return none
	}
	p90, _, enough := RouteP90(input.Samples, input.Implementer, input.CompletedJob)
	if !enough || input.DurationMS <= p90 {
		return none
	}
	up := false
	if weeklyOnly {
		up = week.used <= 35 && week.reset.Sub(input.Now) < 84*time.Hour
	} else {
		up = fiveHour.used <= 50 && fiveHour.reset.Sub(input.Now) < 90*time.Minute
	}
	if !up {
		return none
	}
	if week.used >= 80 && week.reset.Sub(input.Now) > 24*time.Hour {
		return none
	}
	return GearAdviceResult{Advice: GearAdviceUp, WindowID: windowID}
}

type quotaWindowState uint8

const (
	quotaAbsent quotaWindowState = iota
	quotaValid
	quotaInvalid
)

type quotaWindowValue struct {
	used  float64
	reset time.Time
}

func parseGearQuotaWindow(snapshot map[string]any, label string, now time.Time) (quotaWindowValue, quotaWindowState) {
	usedValue, hasUsed := snapshot[label+"_used_pct"]
	resetValue, hasReset := snapshot[label+"_resets_at"]
	if !hasUsed && !hasReset {
		return quotaWindowValue{}, quotaAbsent
	}
	used, ok := usedValue.(float64)
	resetText, resetOK := resetValue.(string)
	if !ok || !resetOK || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return quotaWindowValue{}, quotaInvalid
	}
	reset, err := time.Parse(time.RFC3339Nano, resetText)
	if err != nil || !reset.After(now) {
		return quotaWindowValue{}, quotaInvalid
	}
	return quotaWindowValue{used: used, reset: reset}, quotaValid
}

// ClaimGearAdvice atomically reserves one shared suggestion for a quota source
// and governing window. A different reset timestamp naturally permits a claim.
func ClaimGearAdvice(root, sourceIdentity, windowID string, advice GearAdvice) (bool, error) {
	if root == "" || sourceIdentity == "" || windowID == "" || advice != GearAdviceUp && advice != GearAdviceDown {
		return false, os.ErrInvalid
	}
	key := sha256.Sum256([]byte(sourceIdentity + "\x00" + windowID))
	path := filepath.Join(root, "gear-advice", "claims", hex.EncodeToString(key[:])+".json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	record := map[string]string{"source_identity": sourceIdentity, "window_id": windowID, "advice": string(advice)}
	if err := writeJSONAtomically(path, record); err != nil {
		return false, err
	}
	return true, nil
}
