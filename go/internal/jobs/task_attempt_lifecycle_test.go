package jobs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
)

func TestStartAfterCancelAndDuplicateReplay(t *testing.T) {
	root := scratch(t)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	project := contextio.ProjectKey(repo)
	if err := enforce.SaveTaskAttempt(root, enforce.TaskAttempt{ProjectKey: project, TaskID: "task", LatestJobID: "cancelled-job", TerminalDisposition: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	launches := 0
	opt := StartOptions{StateRoot: root, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true,
		ValidateAgent: func(string) error { return nil }}, Launch: func(string, int64, string) error { launches++; return nil }}
	args := map[string]any{"call": "00000000-0000-4000-8000-000000000001", "prompt": "retry", "mode": "new", "cwd": repo, "taskId": "task"}
	result, job, err := Start(root, args, opt)
	if err != nil || job == nil {
		t.Fatalf("start: %v %#v", err, result)
	}
	attempt, _, err := enforce.LoadTaskAttempt(root, project, "task")
	if err != nil || attempt.TerminalDisposition != "started" || attempt.LatestJobID != recordMap(job)["jobId"] {
		t.Fatalf("new start did not reset cancellation: %#v %v", attempt, err)
	}
	if err := enforce.SaveTaskAttempt(root, enforce.TaskAttempt{ProjectKey: project, TaskID: "task", LatestJobID: "later-job", TerminalDisposition: "applied"}); err != nil {
		t.Fatal(err)
	}
	replayed, _, err := Start(root, args, opt)
	attempt, _, loadErr := enforce.LoadTaskAttempt(root, project, "task")
	if err != nil || loadErr != nil || launches != 1 || !reflect.DeepEqual(result, replayed) || attempt.LatestJobID != "later-job" || attempt.TerminalDisposition != "applied" {
		t.Fatalf("duplicate replay changed result/attempt: %#v %#v launches=%d err=%v/%v", replayed, attempt, launches, err, loadErr)
	}
}

func TestStartAttemptSendBackReplayAndCancelRecovery(t *testing.T) {
	root := scratch(t)
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-retry"
	if err := SaveRecord(filepath.Join(root, "codex_exec_mcp", "threads"), map[string]any{
		"thread_id": threadID, "repoRoot": repo, "cwd": worktree, "worktreePath": worktree, "config_args": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	priorID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeHostTestJSON(t, filepath.Join(jobsDir, priorID+".json"), map[string]any{
		"jobId": priorID, "threadId": threadID, "taskId": "task-1", "state": "SUCCEEDED", "worktreePath": worktree,
		"repoRoot": repo, "route": "mechanical", "model": "gpt-6-luna", "phase": "implementation",
	})
	args := map[string]any{"call": "00000000-0000-4000-8000-000000000001", "prompt": "reply", "mode": "reply",
		"threadId": threadID, "taskId": "task-1", "route": "mechanical", "model": "gpt-6-luna", "phase": "implementation"}
	opt := StartOptions{StateRoot: root, Validation: StartValidationOptions{Agent: "codex", FixedAgent: true,
		ValidateAgent: func(string) error { return nil }}, Launch: func(string, int64, string) error { return nil }}
	started, _, err := Start(root, args, opt)
	if err != nil {
		t.Fatal(err)
	}
	if started["structuredContent"].(map[string]any)["state"] == "FAILED" {
		t.Fatalf("start failed: %#v", started)
	}
	record, status, err := enforce.LoadTaskAttempt(root, contextio.ProjectKey(repo), "task-1")
	if err != nil || status != enforce.TaskAttemptOK || !record.SendBackUsed || record.PredecessorJobID != priorID {
		t.Fatalf("replayed send-back record = %#v status=%s err=%v", record, status, err)
	}
	childID := started["structuredContent"].(map[string]any)["jobId"].(string)
	writeHostTestJSON(t, filepath.Join(jobsDir, childID+".json"), map[string]any{
		"jobId": childID, "taskId": "task-1", "state": "SUCCEEDED", "worktreePath": worktree, "repoRoot": repo,
		"route": "mechanical", "model": "gpt-6-luna", "phase": "implementation",
	})
	if _, _, err := Start(root, map[string]any{"call": "00000000-0000-4000-8000-000000000002", "prompt": "redispatch", "mode": "new",
		"cwd": repo, "taskId": "task-1", "route": "mechanical", "model": "gpt-6-luna", "phase": "implementation"}, opt); err != nil {
		t.Fatal(err)
	}
	record, _, err = enforce.LoadTaskAttempt(root, contextio.ProjectKey(repo), "task-1")
	if err != nil || record.Failure == nil || record.Failure.Category != "rejected_after_sendback" {
		t.Fatalf("durably rejected reply was not latched: %#v err=%v", record, err)
	}

	for _, reason := range []string{"failed", "stopped", "superseded"} {
		jobID := map[string]string{"failed": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"stopped":    "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			"superseded": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}[reason]
		taskID := "task-cancel-" + reason
		path := filepath.Join(jobsDir, jobID+".json")
		writeHostTestJSON(t, path, map[string]any{"jobId": jobID, "taskId": taskID, "state": "SUCCEEDED",
			"worktreePath": worktree, "repoRoot": repo, "route": "mechanical", "model": "gpt-6-luna",
			"phase": "implementation"})
		Cancel(root, map[string]any{"jobId": jobID, "reason": reason})
		got, state, loadErr := enforce.LoadTaskAttempt(root, contextio.ProjectKey(repo), taskID)
		if loadErr != nil || state != enforce.TaskAttemptOK {
			t.Fatalf("cancel recovery: state=%s err=%v", state, loadErr)
		}
		if reason == "failed" && (got.Failure == nil || got.Failure.JobID != jobID) {
			t.Fatalf("failed cancel did not latch: %#v", got)
		}
		if reason != "failed" && got.Failure != nil {
			t.Fatalf("ordinary disposition latched failure: %#v", got.Failure)
		}
	}
}

func TestTaskAttemptRejectedRedispatchNonLunaDoesNotLatchFailure(t *testing.T) {
	root := scratch(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	project := contextio.ProjectKey(repo)
	if err := enforce.SaveTaskAttempt(root, enforce.TaskAttempt{ProjectKey: project, TaskID: "task",
		SendBackUsed: true}); err != nil {
		t.Fatal(err)
	}
	job := map[string]any{"taskId": "task", "jobId": "redispatch", "repoRoot": repo,
		"decisionRecorded": true, "phase": "implementation", "route": "consequential", "model": "gpt-6-sol"}
	if err := persistRejectedTaskAttempt(root, job); err != nil {
		t.Fatal(err)
	}
	got, status, err := enforce.LoadTaskAttempt(root, project, "task")
	if err != nil || status != enforce.TaskAttemptOK || got.TerminalDisposition != "rejected" || got.Failure != nil {
		t.Fatalf("non-luna rejection attempt = %#v status=%s err=%v", got, status, err)
	}
}
