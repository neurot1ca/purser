package jobs

import (
	"path/filepath"
	"runtime"
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
			hostAlive := false
			if state == "RUNNING" {
				pid, pidOK := recoveryInteger(record, "hostPid")
				created, createdOK := recoveryInteger(record, "hostCreatedAt")
				contained := runtime.GOOS != "windows" || job["hostContained"] == true
				hostAlive = pidOK && pid > 0 && createdOK && contained &&
					recoveryProcessState(int(pid), uint64(created)) == "ALIVE"
			}
			if state == "QUEUED" && job["leaseQueued"] == true && acknowledged || state == "RUNNING" && (hostAlive || acknowledged) {
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
	if finishUnconfirmedLaunch(stateRoot, jobID) {
		return finishResult(map[string]any{"jobId": jobID, "state": "FAILED",
			"errorCode": "HOST_LAUNCH_UNCONFIRMED", "message": "Host launch was not acknowledged and the recorded host is no longer alive"},
			"jobId", "state", "errorCode", "message")
	}
	return finishResult(map[string]any{"jobId": jobID, "state": "UNKNOWN_OR_IN_PROGRESS",
		"errorCode": "PROCESS_UNCONFIRMED", "message": "Host ownership was not acknowledged"},
		"jobId", "state", "errorCode", "message")
}

func finishUnconfirmedLaunch(stateRoot, jobID string) bool {
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	record := readJob(dir, jobID)
	if record == nil {
		return false
	}
	values := recordMap(record)
	if values["state"] != "QUEUED" {
		return false
	}
	if _, started := values["startedAt"]; started {
		return false
	}
	pidValue, pidOK := recoveryInteger(record, "hostPid")
	created, createdOK := recoveryInteger(record, "hostCreatedAt")
	pid := int(pidValue)
	if !pidOK || !createdOK || pidValue <= 0 || int64(pid) != pidValue || created <= 0 ||
		runtime.GOOS == "windows" && !truthy(values["hostContained"]) ||
		IdentityMatches(pid, created) || recoveryProcessState(pid, uint64(created)) != "DEAD" {
		return false
	}
	if err := finishJob(dir, jobID, "FAILED", "HOST_LAUNCH_UNCONFIRMED", nil,
		"Host launch was not acknowledged and the recorded host is no longer alive",
		float64(time.Now().UnixNano())/1e9, FinishOptions{}); err != nil {
		return false
	}
	final := recordMap(readJob(dir, jobID))
	return final["state"] == "FAILED" && final["errorCode"] == "HOST_LAUNCH_UNCONFIRMED"
}
