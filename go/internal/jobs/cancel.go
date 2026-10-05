package jobs

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
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
		return jobResult(map[string]any{"jobId": id, "state": state, "discarded": true})
	}
	worktree, _ := values["worktreePath"].(string)
	if terminalJobState(state) && worktree != "" && values["applyState"] != "applied" {
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
				if _, exists := m["discardedAt"]; !exists {
					_ = current.Set("discardedAt", float64(time.Now().UnixNano())/1e9)
				}
				recordDecision(m, "rejected", "discard", id)
				recordScopeOutcome(recordMap(current), "discarded")
				if task, _ := m["taskId"].(string); task != "" {
					_ = current.Set("decisionRecorded", true)
				}
				if err = Write(filepath.Join(dir, id+".json"), current); err != nil {
					_ = lock.Release()
					return jobUpdateFailed(id, state)
				}
			}
		}
		_ = lock.Release()
		_ = releaseScopeLease(stateRoot, id)
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
			return jobResult(map[string]any{"jobId": id, "state": state, "errorCode": "CANCELLED", "message": cancelMessage})
		}
	}
	if terminalJobState(state) {
		return cancelRecordResult(values)
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
		if currentState == "CANCELLED" {
			return cancelRecordResult(recordMap(current))
		}
	}
	return processUnconfirmed(id)
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
	case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "UNKNOWN_OR_IN_PROGRESS":
		return true
	default:
		return false
	}
}
