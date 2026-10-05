package jobs

import (
	"errors"
	"path/filepath"
	"time"
)

// RunHostGeneration claims a queued generation and records an explicit failure
// until the Go worker execution loop is assembled.
func RunHostGeneration(dir, jobID string, generation int64, token string) (map[string]any, error) {
	if len(jobID) != 64 || !validJobID(jobID) || generation < 1 || token == "" {
		return nil, errors.New("invalid host generation identity")
	}
	lock, err := AcquireOwnership(dir, jobID)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	path := filepath.Join(dir, jobID+".json")
	job, err := Read(path)
	if err != nil {
		return nil, err
	}
	var recordedID, recordedToken, state string
	var recordedGeneration int64
	if !job.Get("jobId", &recordedID) || recordedID != jobID ||
		!job.Get("generation", &recordedGeneration) || recordedGeneration != generation ||
		!job.Get("ownershipToken", &recordedToken) || recordedToken != token ||
		!job.Get("state", &state) || state != "QUEUED" {
		return map[string]any{"status": "stale"}, nil
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if err = job.Set("state", "FAILED"); err == nil {
		err = job.Set("finishedAt", now)
	}
	if err == nil {
		err = job.Set("errorCode", "GO_HOST_EXECUTION_UNAVAILABLE")
	}
	if err == nil {
		err = job.Set("message", "Go host worker execution is not ported")
	}
	if err == nil {
		err = Write(path, job)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "error", "errorCode": "GO_HOST_EXECUTION_UNAVAILABLE", "message": "Go host worker execution is not ported"}, nil
}

func validJobID(value string) bool {
	for i := range value {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
