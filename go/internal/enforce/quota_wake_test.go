package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestQuotaFailureRecordsWakeAndOtherFailureDoesNot(t *testing.T) {
	root, repo, quotaHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", quotaHome)
	now := time.Now().UTC()
	resets := []time.Time{now.Add(time.Hour), now.Add(48 * time.Hour)}
	limits := map[string]any{
		"primary":   map[string]any{"window_minutes": 300, "used_percent": 100.0, "resets_at": float64(resets[0].Unix())},
		"secondary": map[string]any{"window_minutes": 10080, "used_percent": 100.0, "resets_at": float64(resets[1].Unix())},
	}
	line, err := json.Marshal(map[string]any{"timestamp": now.Format(time.RFC3339), "payload": map[string]any{
		"rate_limits": limits,
	}})
	if err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(quotaHome, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err = os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(day, "rollout-quota.jsonl"), append(line, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	pending := map[string]any{"repo_root": repo, "session_id": "session-quota", "task_id": "task-quota"}
	index := map[string]any{"session_id": "session-quota"}
	job := map[string]any{"state": "FAILED", "launchFailureEvidence": map[string]any{"type": "CREDIT_EXHAUSTED"}}
	if err := recordQuotaWakeFailure(root, pending, index, job); err != nil {
		t.Fatal(err)
	}
	wake, err := contextio.ReadQuotaWake(repo, root, now)
	if err != nil || wake == nil || wake.ResetAt != resets[1].Format(time.RFC3339) || wake.TaskID != "task-quota" || wake.Reason != "quota_failure" {
		t.Fatalf("quota failure wake = %#v, %v", wake, err)
	}
	if err = contextio.ClearQuotaWake(repo, root); err != nil {
		t.Fatal(err)
	}
	job["launchFailureEvidence"] = map[string]any{"type": "AMBIGUOUS"}
	if err = recordQuotaWakeFailure(root, pending, index, job); err != nil {
		t.Fatal(err)
	}
	wake, err = contextio.ReadQuotaWake(repo, root, now)
	if err != nil || wake != nil {
		t.Fatalf("other failure wake = %#v, %v", wake, err)
	}
}
