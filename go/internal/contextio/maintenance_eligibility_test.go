package contextio

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceEligibilityPythonOracle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "maintenance_reports_oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Command               string                     `json:"command"`
			GuardedRunReportCalls int                        `json:"guarded_run_report_calls"`
			GuardedJobStartCalls  int                        `json:"guarded_job_start_calls"`
			RealJobStartCalls     int                        `json:"real_job_start_calls"`
			State                 map[string]json.RawMessage `json:"state"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	checks := map[string]struct {
		eligible bool
		reason   string
	}{
		"check-not-opted-in":    {false, "not_opted_in"},
		"check-quiet-period":    {false, "quiet_period"},
		"check-cadence":         {false, "cadence"},
		"check-stale-quota":     {false, "stale_quota"},
		"check-active-user-job": {false, "active_user_job"},
		"check-eligible":        {true, "eligible"},
	}
	seen := 0
	for _, tc := range fixture.Cases {
		want, relevant := checks[tc.Command]
		if !relevant {
			continue
		}
		seen++
		if (tc.GuardedRunReportCalls > 0) != want.eligible || tc.GuardedJobStartCalls != tc.GuardedRunReportCalls || tc.RealJobStartCalls != 0 {
			t.Fatalf("fixture guard evidence is inconsistent for %s", tc.Command)
		}
		t.Run(tc.Command, func(t *testing.T) {
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			repo, state := t.TempDir(), t.TempDir()
			if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			// A real git root is needed to exercise the same canonical-root gate.
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			maintenance := filepath.Join(state, "projects", ProjectKey(repo), "maintenance")
			if tc.Command != "check-not-opted-in" {
				writeMaintenanceFixture(t, filepath.Join(maintenance, "opt-in.json"), map[string]any{"project_root": repo, "enabled": true})
				writeMaintenanceFixture(t, filepath.Join(maintenance, "consent.json"), map[string]any{"project_root": repo, "decision": "yes"})
			}
			activityAge := int64(maintenanceQuietPeriod.Seconds() + 1)
			runAge := int64(maintenanceCadence.Seconds() + 1)
			switch tc.Command {
			case "check-quiet-period":
				activityAge = 0
			case "check-cadence":
				runAge = 0
			}
			if tc.Command != "check-not-opted-in" {
				writeMaintenanceFixture(t, filepath.Join(maintenance, "last-activity.json"), map[string]any{"at": float64(now.Unix()) - float64(activityAge)})
				completed := float64(now.Unix()) - float64(runAge)
				writeMaintenanceFixture(t, filepath.Join(maintenance, "last-run.json"), map[string]any{"completed_at": completed})
				writeMaintenanceFixture(t, filepath.Join(maintenance, "reports", "test-gaps", "last-run.json"), map[string]any{"completed_at": completed})
			}
			if tc.Command == "check-active-user-job" {
				writeMaintenanceFixture(t, filepath.Join(state, "codex_exec_mcp", "jobs", "active.json"), map[string]any{"state": "RUNNING"})
			}
			capturedAt := now.Format(time.RFC3339Nano)
			if tc.Command == "check-stale-quota" {
				capturedAt = now.Add(-maintenanceQuotaAge - time.Second).Format(time.RFC3339Nano)
			}
			quota := map[string]any{"snapshot_at": capturedAt, "5h_used_pct": float64(1), "5h_resets_at": "cycle-a"}
			result := MaintenanceEligible(repo, state, now, quota, "cleanup")
			if result.Eligible != want.eligible || result.Reason != want.reason {
				t.Fatalf("eligibility = (%v, %q), want (%v, %q)", result.Eligible, result.Reason, want.eligible, want.reason)
			}
			if tc.Command == "check-eligible" {
				selected := SelectMaintenanceReportType(maintenance, []string{"cleanup", "test-gaps"})
				wantType := ""
				for path, raw := range tc.State {
					if strings.HasPrefix(path, "codex_exec_mcp/jobs/") {
						var job struct {
							ReportType string `json:"report_type"`
						}
						if json.Unmarshal(raw, &job) == nil {
							wantType = job.ReportType
						}
					}
				}
				if selected == "" || selected != wantType {
					t.Fatalf("selected report type = %q, want %q from Python fixture", selected, wantType)
				}
			}
		})
	}
	if seen != len(checks) {
		t.Fatalf("fixture supplied %d of %d maintenance eligibility cases", seen, len(checks))
	}
}

func TestMaintenanceEligibilityNonNumericCompletionIsNotDue(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo, state := t.TempDir(), t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	root, err := canonicalMaintenanceRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	maintenance := filepath.Join(state, "projects", ProjectKey(root), "maintenance")
	writeMaintenanceFixture(t, filepath.Join(maintenance, "opt-in.json"), map[string]any{"project_root": root, "enabled": true})
	writeMaintenanceFixture(t, filepath.Join(maintenance, "last-activity.json"), map[string]any{"at": float64(now.Unix() - int64(maintenanceQuietPeriod.Seconds()) - 1)})
	writeMaintenanceFixture(t, filepath.Join(maintenance, "last-run.json"), map[string]any{"completed_at": "not-a-number"})
	quota := map[string]any{"snapshot_at": now.Format(time.RFC3339Nano), "5h_used_pct": float64(1), "5h_resets_at": "cycle-a"}
	result := MaintenanceEligible(repo, state, now, quota, "cleanup")
	if result.Eligible {
		t.Fatalf("non-numeric completed_at made maintenance eligible: %+v", result)
	}
}

func TestCanonicalMaintenanceRootUsesResolvedPath(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalMaintenanceRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	want := resolve(repo)
	if got != want {
		t.Fatalf("canonical root = %q, want resolved root %q", got, want)
	}
}

func writeMaintenanceFixture(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
