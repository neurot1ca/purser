package jobs

import (
	"os"
	"path/filepath"
	"time"
)

var maxJobRecords = 256

// RecoverPending reconciles bounded, valid nonterminal job records with their hosts.
func RecoverPending(stateRoot string, now time.Time) (int, error) {
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	actions := 0
	for _, entry := range entries {
		if actions >= maxJobRecords {
			break
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))]
		if len(id) != 64 || !validJobID(id) {
			continue
		}
		record, readErr := Read(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var recordedID, state string
		if !record.Get("jobId", &recordedID) || recordedID != id || !record.Get("state", &state) || recoveryTerminal(state) {
			continue
		}
		RecoverJob(stateRoot, id, float64(now.UnixNano())/1e9, RecoverOptions{})
		actions++
	}
	return actions, nil
}

func recoveryTerminal(state string) bool {
	switch state {
	case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS", "EXPIRED":
		return true
	default:
		return false
	}
}
