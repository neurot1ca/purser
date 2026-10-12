package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"purser-hook/internal/contextio"
	"purser-hook/internal/models"
)

const (
	TaskAttemptMissing = "MISSING"
	TaskAttemptOK      = "OK"
	TaskAttemptCorrupt = "CORRUPT"
)

type TaskFailureInput struct {
	ProjectKey, TaskID, JobID   string
	Phase, Route, Model         string
	Outcome, CancellationReason string
	SendBackUsed                bool
}

type TaskFailureEvidence struct {
	JobID    string `json:"job_id"`
	Category string `json:"category"`
	Model    string `json:"model,omitempty"`
}

type TaskAttempt struct {
	ProjectKey          string               `json:"project_key"`
	TaskID              string               `json:"task_id"`
	LatestJobID         string               `json:"latest_job_id,omitempty"`
	PredecessorJobID    string               `json:"predecessor_job_id,omitempty"`
	SendBackUsed        bool                 `json:"send_back_used,omitempty"`
	TerminalDisposition string               `json:"terminal_disposition,omitempty"`
	Failure             *TaskFailureEvidence `json:"failure,omitempty"`
}

func classifyTaskFailure(in TaskFailureInput) (TaskFailureEvidence, bool) {
	if in.ProjectKey == "" || in.TaskID == "" || in.JobID == "" || in.Phase != "implementation" ||
		(in.Route != "subtle" && in.Route != "mechanical") || !models.SameFamily(in.Model, "gpt-6-luna") {
		return TaskFailureEvidence{}, false
	}
	if in.Outcome == "rejected" && in.SendBackUsed {
		return TaskFailureEvidence{JobID: in.JobID, Category: "rejected_after_sendback", Model: in.Model}, true
	}
	if (in.Outcome == "cancelled" || in.Outcome == "discarded") && in.CancellationReason == "failed" {
		return TaskFailureEvidence{JobID: in.JobID, Category: "failed_abandonment", Model: in.Model}, true
	}
	return TaskFailureEvidence{}, false
}

func taskAttemptPath(root, projectKey, taskID string) string {
	key := sha256.Sum256([]byte(projectKey + "\x00" + taskID))
	return filepath.Join(root, "task_retries", hex.EncodeToString(key[:])+".json")
}

// LoadTaskAttempt reads only the project/task record. Corrupt content is
// distinct from an absent record so callers can fail closed.
func LoadTaskAttempt(root, projectKey, taskID string) (TaskAttempt, string, error) {
	if root == "" || projectKey == "" || taskID == "" {
		return TaskAttempt{}, TaskAttemptCorrupt, errors.New("task attempt identity is incomplete")
	}
	path := taskAttemptPath(root, projectKey, taskID)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return TaskAttempt{}, TaskAttemptMissing, nil
	}
	if err != nil {
		return TaskAttempt{}, TaskAttemptCorrupt, err
	}
	var record TaskAttempt
	if json.Unmarshal(raw, &record) != nil || record.ProjectKey != projectKey || record.TaskID != taskID {
		return TaskAttempt{}, TaskAttemptCorrupt, nil
	}
	return record, TaskAttemptOK, nil
}

// SaveTaskAttempt serializes updates and keeps failure evidence sticky across
// later attempt records. The record is self-contained after job cleanup.
func SaveTaskAttempt(root string, update TaskAttempt) error {
	if root == "" || update.ProjectKey == "" || update.TaskID == "" {
		return errors.New("task attempt identity is incomplete")
	}
	path := taskAttemptPath(root, update.ProjectKey, update.TaskID)
	unlock, err := contextio.LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	current, status, err := LoadTaskAttempt(root, update.ProjectKey, update.TaskID)
	if err != nil {
		return err
	}
	if status == TaskAttemptCorrupt {
		return errors.New("task attempt record is corrupt")
	}
	if status == TaskAttemptMissing {
		current = TaskAttempt{ProjectKey: update.ProjectKey, TaskID: update.TaskID}
	}
	if update.LatestJobID != "" {
		current.LatestJobID = update.LatestJobID
	}
	if update.PredecessorJobID != "" {
		current.PredecessorJobID = update.PredecessorJobID
	}
	if update.TerminalDisposition != "" {
		current.TerminalDisposition = update.TerminalDisposition
	}
	current.SendBackUsed = current.SendBackUsed || update.SendBackUsed
	if current.Failure == nil && update.Failure != nil && update.Failure.JobID != "" &&
		(update.Failure.Category == "rejected_after_sendback" || update.Failure.Category == "failed_abandonment") {
		failure := *update.Failure
		current.Failure = &failure
	}
	return atomicAdmissionJSON(path, current)
}
