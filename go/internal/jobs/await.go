package jobs

import (
	"path/filepath"
	"time"
)

const hostAcknowledgementTimeout = 10 * time.Second

// AwaitHost mirrors the Python bridge's bounded wait for durable host ownership.
func AwaitHost(stateRoot, jobID string, _ *Record) map[string]any {
	return awaitHost(stateRoot, jobID, hostAcknowledgementTimeout)
}

func awaitHost(stateRoot, jobID string, timeout time.Duration) map[string]any {
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	until := time.Now().Add(timeout)
	for time.Now().Before(until) {
		if record := readJob(dir, jobID); record != nil {
			job := recordMap(record)
			state, _ := job["state"].(string)
			if state == "UNKNOWN_OR_IN_PROGRESS" {
				return finishResult(map[string]any{"jobId": jobID, "state": state,
					"errorCode": "PROCESS_UNCONFIRMED", "message": "Host ownership or child state unconfirmed"},
					"jobId", "state", "errorCode", "message")
			}
			port, portOK := job["signalPort"].(float64)
			token, _ := job["signalToken"].(string)
			acknowledged := false
			if portOK && port > 0 && token != "" {
				acknowledged, _ = SignalPing("127.0.0.1:"+pythonString(port), jobID, token, 500*time.Millisecond)
			}
			if state == "QUEUED" && job["leaseQueued"] == true && acknowledged || state == "RUNNING" && acknowledged {
				return finishResult(map[string]any{"jobId": job["jobId"], "state": state,
					"acceptedAt": job["acceptedAt"], "deadlineAt": job["deadlineAt"]},
					"jobId", "state", "acceptedAt", "deadlineAt")
			}
			if terminalJobs[state] {
				return finishResult(map[string]any{"jobId": job["jobId"], "acceptedAt": job["acceptedAt"],
					"deadlineAt": job["deadlineAt"], "state": "RUNNING"},
					"jobId", "acceptedAt", "deadlineAt", "state")
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return finishResult(map[string]any{"jobId": jobID, "state": "UNKNOWN_OR_IN_PROGRESS",
		"errorCode": "PROCESS_UNCONFIRMED", "message": "Host ownership was not acknowledged"},
		"jobId", "state", "errorCode", "message")
}
