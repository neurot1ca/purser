package jobs

import (
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
	"purser-hook/internal/models"
)

const cancelMessage = "Codex cancelled"

var cancelSignalTimeout = 5 * time.Second

type CancelOptions struct {
	RecordDecision             func(job map[string]any, decision, kind, sourceJobID string)
	RecordScopeConflictOutcome func(job map[string]any, action string)
	ReleaseScopeLease          func(stateRoot, jobID string) bool
}

// Cancel requests cancellation of a queued or running async job.
func Cancel(stateRoot string, args map[string]any, options ...CancelOptions) map[string]any {
	var opt CancelOptions
	if len(options) > 0 {
		opt = options[0]
	}
	recordDecision := opt.RecordDecision
	if recordDecision == nil {
		recordDecision = func(job map[string]any, decision, kind, source string) {
			enforce.RecordClaudeDecision(stateRoot, job, decision, kind, source)
		}
	}
	recordScopeOutcome := opt.RecordScopeConflictOutcome
	if recordScopeOutcome == nil {
		recordScopeOutcome = func(job map[string]any, action string) {
			enforce.RecordScopeConflictOutcome(stateRoot, job, action)
		}
	}
	releaseScopeLease := opt.ReleaseScopeLease
	if releaseScopeLease == nil {
		releaseScopeLease = lease.ReleaseJobScopeLease
	}
	id, ok := args["jobId"].(string)
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	if !ok || id == "" || strings.Trim(id, "0123456789abcdef") != "" {
		return jobResult(map[string]any{"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
	}
	job := readJob(dir, id)
	if job == nil {
		return jobResult(map[string]any{"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
	}
	values := recordMap(job)
	state, _ := values["state"].(string)
	if values["discarded"] == true {
		if err := persistCancellationAttempt(stateRoot, values); err != nil {
			log.Printf("task attempt persistence failed for cancelled job %s: %v", id, err)
		}
		dispatchWorktreeRetirement(stateRoot, id)
		return jobResult(map[string]any{"jobId": id, "state": state, "discarded": true})
	}
	worktree, _ := values["worktreePath"].(string)
	if terminalJobState(state) && worktree != "" && values["applyState"] != "applied" {
		discardedPersisted := false
		lock, err := AcquireOwnership(dir, id)
		if err != nil {
			return jobUpdateFailed(id, state)
		}
		current := readJob(dir, id)
		if current != nil {
			m := recordMap(current)
			currentState, _ := m["state"].(string)
			currentWorktree, _ := m["worktreePath"].(string)
			if terminalJobState(currentState) && currentWorktree != "" && m["applyState"] != "applied" {
				_ = current.Set("discarded", true)
				if reason, ok := args["reason"].(string); ok {
					_ = current.Set("cancellationReason", reason)
				}
				if _, exists := m["discardedAt"]; !exists {
					_ = current.Set("discardedAt", float64(time.Now().UnixNano())/1e9)
				}
				if task, _ := m["taskId"].(string); task != "" {
					_ = current.Set("decisionRecorded", true)
				}
				if err = Write(filepath.Join(dir, id+".json"), current); err != nil {
					_ = lock.Release()
					return jobUpdateFailed(id, state)
				}
				m = recordMap(current)
				recordDecision(m, "rejected", "discard", id)
				recordScopeOutcome(m, "discarded")
				if err = persistCancellationAttempt(stateRoot, m); err != nil {
					log.Printf("task attempt persistence failed for discarded job %s: %v", id, err)
				}
				discardedPersisted = true
			}
		}
		_ = lock.Release()
		_ = releaseScopeLease(stateRoot, id)
		if discardedPersisted {
			dispatchWorktreeRetirement(stateRoot, id)
		}
		return jobResult(map[string]any{"jobId": id, "state": state, "discarded": true})
	}
	if values["leaseQueued"] == true && state == "QUEUED" {
		lock, err := AcquireOwnership(dir, id)
		if err != nil {
			return processUnconfirmed(id)
		}
		current := readJob(dir, id)
		if current != nil {
			var currentState string
			current.Get("state", &currentState)
			if currentState == "QUEUED" {
				finishCancelled(current)
				if reason, ok := args["reason"].(string); ok {
					_ = current.Set("cancellationReason", reason)
				}
				if err = Write(filepath.Join(dir, id+".json"), current); err != nil {
					_ = lock.Release()
					return processUnconfirmed(id)
				}
				recordScopeOutcome(recordMap(current), "discarded")
				values = recordMap(current)
				state = "CANCELLED"
			}
		}
		_ = lock.Release()
		if state == "CANCELLED" {
			if err := persistCancellationAttempt(stateRoot, values); err != nil {
				log.Printf("task attempt persistence failed for cancelled job %s: %v", id, err)
			}
			stopRecordedHost(current)
			dispatchWorktreeRetirement(stateRoot, id)
			return jobResult(map[string]any{"jobId": id, "state": state, "errorCode": "CANCELLED", "message": cancelMessage})
		}
	}
	if terminalJobState(state) {
		if state == "CANCELLED" {
			if reason, ok := args["reason"].(string); ok {
				values["cancellationReason"] = reason
			}
			if err := persistCancellationAttempt(stateRoot, values); err != nil {
				log.Printf("task attempt persistence failed for cancelled job %s: %v", id, err)
			}
			dispatchWorktreeRetirement(stateRoot, id)
		}
		return cancelRecordResult(values)
	}
	if state == "RUNNING" && deadHostTreeConfirmed(job) {
		lock, err := AcquireOwnership(dir, id)
		if err != nil {
			return processUnconfirmed(id)
		}
		current := readJob(dir, id)
		currentState := ""
		if current != nil {
			current.Get("state", &currentState)
		}
		if current != nil && currentState == "RUNNING" && deadHostTreeConfirmed(current) {
			finishCancelled(current)
			if reason, ok := args["reason"].(string); ok {
				_ = current.Set("cancellationReason", reason)
			}
			if err = Write(filepath.Join(dir, id+".json"), current); err != nil {
				_ = lock.Release()
				return processUnconfirmed(id)
			}
			values = recordMap(current)
			state = "CANCELLED"
		}
		_ = lock.Release()
		if state == "CANCELLED" {
			if err := persistCancellationAttempt(stateRoot, values); err != nil {
				log.Printf("task attempt persistence failed for cancelled job %s: %v", id, err)
			}
			dispatchWorktreeRetirement(stateRoot, id)
			return cancelRecordResult(values)
		}
	}
	port, portOK := values["signalPort"].(float64)
	token, tokenOK := values["signalToken"].(string)
	if !portOK || !tokenOK || port <= 0 || port > 65535 {
		return processUnconfirmed(id)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	confirmed, err := SignalCancel(address, id, token, cancelSignalTimeout)
	if err != nil || !confirmed {
		return processUnconfirmed(id)
	}
	current := readJob(dir, id)
	if current != nil {
		var currentState string
		current.Get("state", &currentState)
		if terminalJobState(currentState) {
			m := recordMap(current)
			if currentState == "CANCELLED" {
				if reason, ok := args["reason"].(string); ok {
					m["cancellationReason"] = reason
				}
				if err := persistCancellationAttempt(stateRoot, m); err != nil {
					log.Printf("task attempt persistence failed for cancelled job %s: %v", id, err)
				}
			}
			if currentState == "CANCELLED" {
				dispatchWorktreeRetirement(stateRoot, id)
			}
			return cancelRecordResult(m)
		}
	}
	confirmed, err = SignalWait(address, id, token, 7*time.Second)
	if err != nil || !confirmed {
		return processUnconfirmed(id)
	}
	current = readJob(dir, id)
	if current != nil {
		var currentState string
		current.Get("state", &currentState)
		if terminalJobState(currentState) {
			m := recordMap(current)
			if currentState == "CANCELLED" {
				if reason, ok := args["reason"].(string); ok {
					m["cancellationReason"] = reason
				}
				if err := persistCancellationAttempt(stateRoot, m); err != nil {
					log.Printf("task attempt persistence failed for cancelled job %s: %v", id, err)
				}
			}
			if currentState == "CANCELLED" {
				dispatchWorktreeRetirement(stateRoot, id)
			}
			return cancelRecordResult(m)
		}
	}
	return processUnconfirmed(id)
}

func persistCancellationAttempt(root string, job map[string]any) error {
	taskID, jobID := nonemptyString(job["taskId"]), nonemptyString(job["jobId"])
	repository := nonemptyString(job["repoRoot"])
	if taskID == "" || jobID == "" || repository == "" {
		return nil
	}
	project := contextio.ProjectKey(repository)
	previous, status, err := enforce.LoadTaskAttempt(root, project, taskID)
	if err != nil || status == enforce.TaskAttemptCorrupt {
		if err != nil {
			return err
		}
		return errors.New("task attempt record is corrupt")
	}
	outcome := "cancelled"
	if job["discarded"] == true {
		outcome = "discarded"
	}
	if outcome == "discarded" && previous.SendBackUsed && job["decisionRecorded"] == true && nonemptyString(job["cancellationReason"]) == "" {
		outcome = "rejected"
	}
	input := enforce.TaskFailureInput{ProjectKey: project, TaskID: taskID, JobID: jobID,
		Phase: nonemptyString(job["phase"]), Route: nonemptyString(job["route"]), Model: nonemptyString(job["model"]),
		Outcome: outcome, CancellationReason: nonemptyString(job["cancellationReason"]), SendBackUsed: previous.SendBackUsed}
	var failure *enforce.TaskFailureEvidence
	if lunaImplementationAttempt(job) {
		if input.CancellationReason == "failed" {
			failure = &enforce.TaskFailureEvidence{JobID: jobID, Category: "failed_abandonment", Model: input.Model}
		} else if input.Outcome == "rejected" && input.SendBackUsed {
			failure = &enforce.TaskFailureEvidence{JobID: jobID, Category: "rejected_after_sendback", Model: input.Model}
		}
	}
	disposition := outcome
	if input.CancellationReason == "failed" {
		disposition = "failed"
	}
	return enforce.SaveTaskAttempt(root, enforce.TaskAttempt{ProjectKey: project, TaskID: taskID,
		TerminalDisposition: disposition, Failure: failure})
}

func lunaImplementationAttempt(job map[string]any) bool {
	return nonemptyString(job["phase"]) == "implementation" &&
		(nonemptyString(job["route"]) == "subtle" || nonemptyString(job["route"]) == "mechanical") &&
		models.SameFamily(nonemptyString(job["model"]), "gpt-6-luna")
}

func deadHostTreeConfirmed(record *Record) bool {
	pidValue, pidOK := recoveryInteger(record, "hostPid")
	created, createdOK := recoveryInteger(record, "hostCreatedAt")
	pid := int(pidValue)
	if !pidOK || pidValue <= 0 || int64(pid) != pidValue || !createdOK || created < 0 ||
		(runtime.GOOS == "windows" && !truthy(recordMap(record)["hostContained"])) ||
		recoveryProcessState(pid, uint64(created)) != "DEAD" {
		return false
	}
	if runtime.GOOS != "windows" {
		childValue, ok := recoveryInteger(record, "pid")
		child := int(childValue)
		return ok && childValue > 0 && int64(child) == childValue && recoveryGroupStopped(child)
	}
	return true
}

func stopRecordedHost(record *Record) bool {
	if record == nil {
		return false
	}
	pidValue, pidOK := recoveryInteger(record, "hostPid")
	created, createdOK := recoveryInteger(record, "hostCreatedAt")
	pid := int(pidValue)
	if !pidOK || !createdOK || pidValue <= 0 || int64(pid) != pidValue || created <= 0 || !IdentityMatches(pid, created) {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Kill() == nil
}

func finishCancelled(record *Record) {
	_ = record.Set("state", "CANCELLED")
	_ = record.Set("finishedAt", float64(time.Now().UnixNano())/1e9)
	deleteRecordField(record, "stallDetectedAt")
	var code string
	if record.Get("errorCode", &code) && code == "STALLED" {
		deleteRecordField(record, "errorCode")
	}
	_ = record.Set("errorCode", "CANCELLED")
	_ = record.Set("message", cancelMessage)
	var count any
	if !record.Get("boardDisplayCount", &count) {
		_ = record.Set("boardDisplayCount", 0)
	}
}

func cancelRecordResult(record map[string]any) map[string]any {
	state, _ := record["state"].(string)
	value := map[string]any{"jobId": record["jobId"], "state": state}
	if terminalJobState(state) {
		code, ok := record["errorCode"]
		if !ok {
			code = "PROCESS_UNCONFIRMED"
		}
		message, ok := record["message"]
		if !ok {
			message = "Process state cannot be confirmed"
		}
		value["errorCode"], value["message"] = code, message
	}
	return jobResult(value)
}

func processUnconfirmed(id string) map[string]any {
	return jobResult(map[string]any{"jobId": id, "state": "UNKNOWN_OR_IN_PROGRESS", "errorCode": "PROCESS_UNCONFIRMED", "message": "Process termination unconfirmed"})
}

func jobUpdateFailed(id, state string) map[string]any {
	return jobResult(map[string]any{"jobId": id, "state": state, "errorCode": "JOB_UPDATE_FAILED", "message": "Job record update failed"})
}

func terminalJobState(state string) bool {
	switch state {
	case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS", "SUPERSEDED":
		return true
	default:
		return false
	}
}
