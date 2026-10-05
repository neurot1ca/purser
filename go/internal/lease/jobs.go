package lease

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReleaseJobScopeLease releases the scope lease indexed by an async job.
func ReleaseJobScopeLease(stateRoot, jobID string) bool {
	if jobID == "" || !hexKey(jobID, len(jobID)) {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(stateRoot, "async_jobs", jobID+".json"))
	if err != nil {
		return false
	}
	var index map[string]any
	if json.Unmarshal(raw, &index) != nil || index["jobId"] != jobID {
		return false
	}
	project, _ := index["project_key"].(string)
	owner, _ := index["call_key"].(string)
	if !hexKey(project, 16) || !hexKey(owner, 64) {
		return false
	}
	result := New(filepath.Join(stateRoot, "projects", project)).Release(owner, float64(time.Now().UnixNano())/1e9)
	return result.Status == "OK" || result.Code == "LEASE_OWNER_MISSING"
}

// QueueWaitMS returns the persisted queued-to-acquired interval for telemetry.
func (s *Store) QueueWaitMS(ownerKey string) *int64 {
	release, err := Lock(s.lockPath)
	if err != nil {
		return nil
	}
	defer release()
	value, err := s.read()
	if err != nil {
		return nil
	}
	owners, _ := value["owners"].(map[string]any)
	owner, _ := owners[ownerKey].(map[string]any)
	queued, qok := pythonFloat(owner["queued_at"])
	acquired, aok := pythonFloat(owner["acquired_at"])
	if !qok || !aok || math.IsNaN(queued) || math.IsInf(queued, 0) || math.IsNaN(acquired) || math.IsInf(acquired, 0) {
		return nil
	}
	ms := int64(math.RoundToEven((acquired - queued) * 1000))
	if ms < 0 {
		ms = 0
	}
	return &ms
}

func hexKey(value string, length int) bool {
	return len(value) == length && strings.Trim(value, "0123456789abcdef") == ""
}
