package contextio

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFallbackQuotaDeclineRecordsWake(t *testing.T) {
	root, repository := t.TempDir(), t.TempDir()
	state := defaultActiveCoderState()
	state["triggering_evidence"] = map[string]any{
		"type": "QUOTA", "trigger_id": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"question_session": "session-decline", "question_shown": true, "answerable": true,
	}
	if err := writeProjectActiveCoder(repository, root, state); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	changed, err := ApplyFallbackConsent(repository, root, "session-decline", false, reset, nil)
	if err != nil || !changed {
		t.Fatalf("decline = %v, %v", changed, err)
	}
	wake, err := ReadQuotaWake(repository, root, time.Now())
	if err != nil || wake == nil || wake.SessionID != "session-decline" || wake.Reason != "fallback_declined" || wake.ResetAt != reset {
		t.Fatalf("wake = %#v, %v", wake, err)
	}
}

func TestReadQuotaWakeClearsStaleRequest(t *testing.T) {
	root, repository := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	reset := now.Add(-25 * time.Hour)
	if err := RecordQuotaWake(repository, root, "session-stale", "task", reset.Format(time.RFC3339), "quota_failure", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	wake, err := ReadQuotaWake(repository, root, now)
	if err != nil || wake != nil {
		t.Fatalf("stale wake = %#v, %v", wake, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(activeCoderPath(repository, root)), "quota-wake.json")); !os.IsNotExist(err) {
		t.Fatalf("stale wake file still exists: %v", err)
	}
}
