package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type QuotaWake struct {
	ResetAt   string `json:"reset_at"`
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id,omitempty"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
}

func quotaWakePath(project, root string) string {
	return filepath.Join(filepath.Dir(activeCoderPath(project, root)), "quota-wake.json")
}

// RecordQuotaWake replaces the project's single pending wake request.
func RecordQuotaWake(project, root, sessionID, taskID, resetAt, reason string, now time.Time) error {
	reset, err := time.Parse(time.RFC3339, resetAt)
	if err != nil || sessionID == "" || reason != "fallback_declined" && reason != "quota_failure" {
		return os.ErrInvalid
	}
	wake := QuotaWake{ResetAt: reset.UTC().Format(time.RFC3339), SessionID: sessionID,
		TaskID: taskID, Reason: reason, CreatedAt: now.UTC().Format(time.RFC3339)}
	path := quotaWakePath(project, root)
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return writeJSONAtomically(path, wake)
}

// ReadQuotaWake returns the pending request and removes it once its reset is stale.
func ReadQuotaWake(project, root string, now time.Time) (*QuotaWake, error) {
	path := quotaWakePath(project, root)
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wake QuotaWake
	if err = json.Unmarshal(data, &wake); err != nil {
		return nil, err
	}
	reset, err := time.Parse(time.RFC3339, wake.ResetAt)
	if err != nil {
		return nil, err
	}
	if wake.SessionID == "" || wake.Reason != "fallback_declined" && wake.Reason != "quota_failure" {
		return nil, os.ErrInvalid
	}
	if now.UTC().Sub(reset) > 24*time.Hour {
		if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, nil
	}
	return &wake, nil
}

// ClearQuotaWake removes the project's pending wake request.
func ClearQuotaWake(project, root string) error {
	path := quotaWakePath(project, root)
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ClearQuotaWakeAfterReset removes a request only after its recorded reset.
func ClearQuotaWakeAfterReset(project, root string, now time.Time) error {
	path := quotaWakePath(project, root)
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var wake QuotaWake
	if err = json.Unmarshal(data, &wake); err != nil {
		return err
	}
	reset, err := time.Parse(time.RFC3339, wake.ResetAt)
	if err != nil {
		return err
	}
	if now.UTC().Sub(reset) > 24*time.Hour || !now.Before(reset) {
		if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}
