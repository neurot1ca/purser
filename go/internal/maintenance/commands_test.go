package maintenance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDelayedCheckUsesPostDelayActivityAndQuota(t *testing.T) {
	oldSleep, oldClock := checkSleep, maintenanceClock
	t.Cleanup(func() { checkSleep, maintenanceClock = oldSleep, oldClock })

	started := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	finished := started.Add(6 * time.Minute)
	checkSleep = func(time.Duration) {}
	maintenanceClock = func() time.Time { return finished }

	repo, state, quotaHome := t.TempDir(), t.TempDir(), t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	root, err := CanonicalRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	directory := MaintenanceDirectory(state, root)
	for path, value := range map[string]any{
		filepath.Join(directory, "opt-in.json"):        map[string]any{"enabled": true, "project_root": root},
		filepath.Join(directory, "last-activity.json"): map[string]any{"at": float64(started.Unix())},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", quotaHome)
	quotaPath := filepath.Join(quotaHome, "sessions", finished.Format("2006"), finished.Format("01"), finished.Format("02"), "rollout-quota.jsonl")
	quotaEvent, _ := json.Marshal(map[string]any{
		"timestamp": finished.Format(time.RFC3339),
		"payload": map[string]any{"rate_limits": map[string]any{
			"primary": map[string]any{"window_minutes": 300, "used_percent": 1.0, "resets_at": float64(finished.Add(time.Hour).Unix())},
		}},
	})
	if err := os.MkdirAll(filepath.Dir(quotaPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quotaPath, append(quotaEvent, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	_, err = DelayedCheck(root, state, 5*time.Minute, Runner{})
	if err == nil || !strings.Contains(err.Error(), "maintenance job operations are required") {
		t.Fatalf("DelayedCheck error = %v, want it to reach report execution after the quiet period", err)
	}
	if _, err := os.Stat(filepath.Join(state, "maintenance", "active.json")); err == nil {
		t.Fatal("check unexpectedly acquired the global maintenance slot")
	}
}
