package jobs

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/ledger"
	"purser-hook/internal/proc"
)

const partialPatchLimit = 4 << 20
const partialResumePrompt = "Partial changes from an interrupted earlier attempt are already applied in this worktree; review and continue from them."

// Called with job ownership held, after the terminal record is durable.
func finishFailedAttempt(dir, id string, job *Record) {
	m := recordMap(job)
	root := filepath.Dir(filepath.Dir(dir))
	repository, task := nonemptyString(m["repoRoot"]), nonemptyString(m["taskId"])
	if repository == "" || task == "" || m["state"] != "FAILED" {
		return
	}
	status := "no worktree snapshot"
	if worktree, snapshot := nonemptyString(m["worktreePath"]), nonemptyString(m["snapshot"]); worktree != "" && snapshot != "" && !truthy(m["decisionRecorded"]) {
		patch, err := ledger.JobPatch(worktree, snapshot)
		switch {
		case err != nil:
			status = "patch capture skipped: " + err.Error()
		case len(patch) > partialPatchLimit:
			status = "patch capture skipped: exceeds 4 MiB"
		case patch == "":
			status = "no partial changes"
		default:
			if err := atomicWrite(filepath.Join(dir, id+".partial.patch"), []byte(patch)); err != nil {
				status = "patch write skipped: " + err.Error()
			} else {
				status = "partial patch saved"
			}
		}
	}
	_ = job.Set("resumeStatus", status)
	_ = Write(filepath.Join(dir, id+".json"), job)
	if err := enforce.SaveTaskAttempt(root, enforce.TaskAttempt{ProjectKey: contextio.ProjectKey(repository), TaskID: task, LatestJobID: id, TerminalDisposition: "failed"}); err != nil {
		log.Printf("failed task attempt persistence for job %s: %v", id, err)
		_ = job.Set("taskAttemptError", err.Error())
		_ = Write(filepath.Join(dir, id+".json"), job)
	}
}

// Plain git apply is atomic on conflict and preserves the new worktree's baseline.
func resumePartialAttempt(dir string, job *Record, worktree string, prompt *string) {
	var prior string
	if !job.Get("resumeJobId", &prior) || len(prior) != 64 || !validJobID(prior) {
		return
	}
	path := filepath.Join(dir, prior+".partial.patch")
	status := "partial patch unavailable"
	if previous := readJob(dir, prior); previous != nil {
		var reason string
		if previous.Get("resumeStatus", &reason) {
			status += ": " + reason
		}
	}
	file, err := os.Open(path)
	if err == nil {
		var patch []byte
		patch, err = io.ReadAll(io.LimitReader(file, partialPatchLimit+1))
		_ = file.Close()
		if err == nil && len(patch) > partialPatchLimit {
			status = "patch resume skipped: exceeds 4 MiB"
		} else if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cmd := proc.Hide(exec.CommandContext(ctx, "git", "-C", worktree, "apply", "--binary", "-"))
			cmd.Stdin = bytes.NewReader(patch)
			failure := tailBuffer{limit: 4096}
			cmd.Stderr = &failure
			err = cmd.Run()
			cancel()
			if err == nil {
				status = "partial patch applied"
				*prompt += "\n" + partialResumePrompt
			} else {
				status = "patch apply skipped: " + err.Error() + ": " + strings.TrimSpace(failure.String())
			}
		}
	}
	if err != nil && !strings.HasPrefix(status, "patch apply skipped:") {
		status += ": " + err.Error()
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		status += "; patch cleanup skipped: " + err.Error()
	}
	_ = job.Set("resumeStatus", status)
	job.Delete("resumeJobId")
}
