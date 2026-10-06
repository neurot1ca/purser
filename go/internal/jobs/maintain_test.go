package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeMaintenanceJob(t *testing.T, root, fileID, jobID, state string) string {
	t.Helper()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileID+".json")
	data := []byte(fmt.Sprintf(`{"jobId":%q,"state":%q}`, jobID, state))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecoverPendingRecoversNonterminalValidRecord(t *testing.T) {
	root := t.TempDir()
	id := fmt.Sprintf("%064x", 1)
	writeMaintenanceJob(t, root, id, id, "QUEUED")

	count, err := RecoverPending(root, time.Unix(2_000_000_000, 0))
	if err != nil || count != 1 {
		t.Fatalf("RecoverPending() = (%d, %v), want (1, nil)", count, err)
	}
	if _, err := os.Stat(filepath.Join(root, "codex_exec_mcp", "jobs", id+".owner-lock")); err != nil {
		t.Fatalf("RecoverJob was not called: %v", err)
	}
}

func TestRecoverPendingLeavesTerminalRecordUntouched(t *testing.T) {
	root := t.TempDir()
	id := fmt.Sprintf("%064x", 2)
	path := writeMaintenanceJob(t, root, id, id, "UNKNOWN_OR_IN_PROGRESS")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	count, err := RecoverPending(root, time.Unix(2_000_000_000, 0))
	if err != nil || count != 0 {
		t.Fatalf("RecoverPending() = (%d, %v), want (0, nil)", count, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("terminal record changed: %q, %v", after, err)
	}
}

func TestRecoverPendingSkipsMismatchedAndInvalidIDs(t *testing.T) {
	root := t.TempDir()
	validID := fmt.Sprintf("%064x", 3)
	writeMaintenanceJob(t, root, validID, fmt.Sprintf("%064x", 4), "QUEUED")
	writeMaintenanceJob(t, root, "not-a-job-id", "not-a-job-id", "QUEUED")

	count, err := RecoverPending(root, time.Unix(2_000_000_000, 0))
	if err != nil || count != 0 {
		t.Fatalf("RecoverPending() = (%d, %v), want (0, nil)", count, err)
	}
}

func TestRecoverPendingBoundsActions(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= maxJobRecords+4; i++ {
		id := fmt.Sprintf("%064x", i)
		writeMaintenanceJob(t, root, id, id, "QUEUED")
	}

	count, err := RecoverPending(root, time.Unix(2_000_000_000, 0))
	if err != nil || count != maxJobRecords {
		t.Fatalf("RecoverPending() = (%d, %v), want (%d, nil)", count, err, maxJobRecords)
	}
}
