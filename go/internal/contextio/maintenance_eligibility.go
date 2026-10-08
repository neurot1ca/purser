package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maintenanceQuietPeriod = 5 * time.Minute
	maintenanceCadence     = 56 * time.Hour
	maintenanceMaxQuota    = 50.0
	maintenanceQuotaAge    = 30 * time.Minute
)

type MaintenanceEligibility struct {
	Eligible   bool
	Reason     string
	ReportType string
}

// MaintenanceEligible evaluates local maintenance evidence only. quota is the
// already-read quota snapshot; this function never fetches quota or starts jobs.
func MaintenanceEligible(repository, state string, now time.Time, quota map[string]any, reportType string) MaintenanceEligibility {
	result := MaintenanceEligibility{Reason: "unknown_evidence", ReportType: reportType}
	if reportType != "cleanup" && reportType != "test-gaps" {
		result.Reason = "unsupported_report_type"
		return result
	}
	root, err := canonicalMaintenanceRoot(repository)
	if err != nil {
		return result
	}
	directory := filepath.Join(state, "projects", ProjectKey(root), "maintenance")
	marker, markerOK := readSmallObject(filepath.Join(directory, "opt-in.json"))
	activity, activityOK := readSmallObject(filepath.Join(directory, "last-activity.json"))
	if !markerOK || !activityOK || marker["enabled"] != true || marker["project_root"] != root {
		result.Reason = "not_opted_in"
		return result
	}
	at, ok := maintenanceNumber(activity["at"])
	if !ok {
		return result
	}
	if now.Sub(time.Unix(0, int64(at*float64(time.Second)))) < maintenanceQuietPeriod {
		result.Reason = "quiet_period"
		return result
	}
	lastPath := filepath.Join(directory, "last-run.json")
	if reportType == "test-gaps" {
		lastPath = filepath.Join(directory, "reports", reportType, "last-run.json")
	}
	if data, err := os.ReadFile(lastPath); err == nil {
		var last any
		if json.Unmarshal(data, &last) == nil {
			object, ok := last.(map[string]any)
			if !ok {
				return result
			}
			completed, ok := maintenanceNumber(object["completed_at"])
			if !ok {
				return result
			}
			if now.Sub(time.Unix(0, int64(completed*float64(time.Second)))) < maintenanceCadence {
				result.Reason = "cadence"
				return result
			}
		}
	}
	if !maintenanceQuotaFresh(quota, now) {
		result.Reason = "stale_quota"
		return result
	}
	_, used, hasWindow := maintenanceQuotaWindow(quota)
	if !hasWindow {
		consent, valid := readSmallObject(filepath.Join(directory, "consent.json"))
		if !valid || consent["decision"] != "yes" || consent["project_root"] != root {
			result.Reason = "consent_required"
			return result
		}
	} else if used > maintenanceMaxQuota {
		result.Reason = "quota_limit"
		return result
	}
	jobsDir := filepath.Join(state, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil && !os.IsNotExist(err) {
		return result
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		job, valid := readSmallObject(filepath.Join(jobsDir, entry.Name()))
		stateName, stateOK := job["state"].(string)
		if !valid || !stateOK {
			return result
		}
		if stateName == "RUNNING" || stateName == "QUEUED" {
			result.Reason = "active_user_job"
			return result
		}
	}
	result.Eligible, result.Reason = true, "eligible"
	return result
}

// SelectMaintenanceReportType mirrors _select_due_report_type for due types.
func SelectMaintenanceReportType(directory string, candidates []string) string {
	selected, selectedAt := "", 0.0
	for i, candidate := range candidates {
		if candidate != "cleanup" && candidate != "test-gaps" {
			continue
		}
		path := filepath.Join(directory, "last-run.json")
		if candidate == "test-gaps" {
			path = filepath.Join(directory, "reports", candidate, "last-run.json")
		}
		lastAt := 0.0
		if last, valid := readSmallObject(path); valid {
			if value, ok := maintenanceNumber(last["completed_at"]); ok {
				lastAt = value
			}
		}
		if selected == "" || lastAt < selectedAt || lastAt == selectedAt && i < indexMaintenanceType(selected, candidates) {
			selected, selectedAt = candidate, lastAt
		}
	}
	return selected
}

func indexMaintenanceType(value string, candidates []string) int {
	for i, candidate := range candidates {
		if candidate == value {
			return i
		}
	}
	return len(candidates)
}

func canonicalMaintenanceRoot(repository string) (string, error) {
	absolute, err := filepath.Abs(repository)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		absolute = resolved
	}
	// Set the command directory explicitly; callers may pass a nested path.
	return gitTopLevel(absolute)
}

func maintenanceNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func maintenanceQuotaWindow(quota map[string]any) (string, float64, bool) {
	for _, label := range []string{"5h", "7d"} {
		used, ok := maintenanceNumber(quota[label+"_used_pct"])
		reset, resetOK := quota[label+"_resets_at"].(string)
		if ok && resetOK && reset != "" {
			return label, used, true
		}
	}
	return "", 0, false
}

func maintenanceQuotaFresh(quota map[string]any, now time.Time) bool {
	if quota == nil {
		return false
	}
	stamp, ok := quota["snapshot_at"].(string)
	if !ok {
		return false
	}
	captured, err := time.Parse(time.RFC3339Nano, strings.ReplaceAll(stamp, "Z", "+00:00"))
	if err != nil || captured.After(now) || now.Sub(captured) > maintenanceQuotaAge {
		return false
	}
	if _, _, hasWindow := maintenanceQuotaWindow(quota); !hasWindow && (quota["windows_seen"] == true || hasAnyMaintenanceWindow(quota)) {
		return false
	}
	return true
}

func hasAnyMaintenanceWindow(quota map[string]any) bool {
	_, five := quota["5h_used_pct"]
	_, week := quota["7d_used_pct"]
	return five || week
}
