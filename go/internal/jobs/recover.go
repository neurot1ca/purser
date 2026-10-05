package jobs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// RecoverOptions injects process evidence and the pass 158 replacement step.
type RecoverOptions struct {
	ProcessAlive func(pid int, createdAt int64) string
	GroupStopped func(pid int) bool
	LaunchHost   func(dir, jobID string, generation int64, token string) error
	NewToken     func() string
	Clock        func() float64
	// Replace overrides the default replacement step.
	Replace func(stateRoot, jobID string, job *Record, now float64) string
}

// RecoverJob classifies whether a durable async job may be replaced.
func RecoverJob(stateRoot, jobID string, now float64, opt RecoverOptions) string {
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	lock, err := AcquireOwnership(dir, jobID)
	if err != nil {
		return "UNCONFIRMED"
	}
	defer func() {
		if lock != nil {
			_ = lock.Release()
		}
	}()
	job := readJob(dir, jobID)
	if job == nil {
		return "UNCHANGED"
	}
	values := recordMap(job)
	state, _ := values["state"].(string)
	if terminalJobs[state] {
		return "UNCHANGED"
	}
	var index map[string]any
	if data, readErr := os.ReadFile(filepath.Join(stateRoot, "async_jobs", jobID+".json")); readErr == nil {
		if json.Unmarshal(data, &index) != nil {
			index = nil
		}
	}
	if index != nil && index["stage"] == "CONSUMED" {
		return "UNCHANGED"
	}
	if state != "RUNNING" && state != "QUEUED" {
		return "UNCHANGED"
	}
	if truthy(values["leaseQueued"]) && state == "QUEUED" && !truthy(values["hostPid"]) {
		if deadline, ok := number(values["deadlineAt"]); ok && now >= deadline {
			_ = lock.Release()
			lock = nil
			return recoverTimedOut(dir, jobID, now)
		}
		return runReplacement(opt, stateRoot, jobID, job, now)
	}
	pidValue, pidOK := recoveryInteger(job, "hostPid")
	created, createdOK := recoveryInteger(job, "hostCreatedAt")
	pid := int(pidValue)
	pidOK = pidOK && pidValue > 0 && int64(pid) == pidValue
	if !pidOK || !createdOK || runtime.GOOS == "windows" && !truthy(values["hostContained"]) {
		return "UNCONFIRMED"
	}
	processState := "UNCONFIRMED"
	if opt.ProcessAlive != nil {
		processState = opt.ProcessAlive(pid, created)
	} else if created >= 0 {
		processState = recoveryProcessState(pid, uint64(created))
	} else {
		processState = "DEAD"
	}
	if processState != "DEAD" {
		return processState
	}
	if truthy(values["leaseQueued"]) && state == "QUEUED" {
		if deadline, ok := number(values["deadlineAt"]); ok && now >= deadline {
			_ = lock.Release()
			lock = nil
			return recoverTimedOut(dir, jobID, now)
		}
		return runReplacement(opt, stateRoot, jobID, job, now)
	}
	ambiguous := func(code, message string) string {
		_ = job.Set("state", "UNKNOWN_OR_IN_PROGRESS")
		_ = job.Set("finishedAt", now)
		_ = job.Set("errorCode", code)
		_ = job.Set("message", message)
		_ = Write(filepath.Join(dir, jobID+".json"), job)
		return "AMBIGUOUS"
	}
	if _, valid := recoveryInteger(job, "generation"); !valid {
		return ambiguous("RECOVERY_PROOF_MISSING", "Host or child-tree termination proof is missing")
	}
	if state == "RUNNING" && (runtime.GOOS != "windows" || opt.GroupStopped != nil) {
		childValue, ok := recoveryInteger(job, "pid")
		child := int(childValue)
		ok = ok && childValue > 0 && int64(child) == childValue
		stopped := opt.GroupStopped
		if stopped == nil {
			stopped = recoveryGroupStopped
		}
		if !ok || !stopped(child) {
			return "UNCONFIRMED"
		}
	}
	if attempts, ok := recoveryAttempts(job); !ok {
		return ambiguous("RECOVERY_PROOF_MISSING", "Recovery attempt history is invalid")
	} else if attempts >= 2 {
		return ambiguous("RECOVERY_ATTEMPTS_EXHAUSTED", "Automatic recovery attempt limit reached")
	}
	if opt.Replace != nil {
		launchData, err := os.ReadFile(filepath.Join(dir, jobID+".launch"))
		var launch map[string]any
		if err != nil || json.Unmarshal(launchData, &launch) != nil {
			return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable")
		}
		name, _ := launch["originalName"].(string)
		if name == "" {
			name, _ = launch["name"].(string)
		}
		thread, exists := values["threadId"]
		if exists && thread != nil {
			if value, ok := thread.(string); !ok || value == "" {
				return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: invalid thread identity")
			}
		}
		if name == "codex-reply" && (!exists || thread == nil || thread == "") {
			return ambiguous("RECOVERY_THREAD_MISSING", "Original reply thread identity is missing")
		}
	}
	return runReplacement(opt, stateRoot, jobID, job, now)
}

func runReplacement(opt RecoverOptions, stateRoot, jobID string, job *Record, now float64) string {
	if opt.Replace != nil {
		return opt.Replace(stateRoot, jobID, job, now)
	}
	return replaceRecoverJob(opt, stateRoot, jobID, job, now)
}

func recoverTimedOut(dir, jobID string, now float64) string {
	_ = finishJob(dir, jobID, "TIMED_OUT", "DEADLINE_EXCEEDED", nil, "", now, FinishOptions{})
	return "TIMED_OUT"
}

func recoveryInteger(record *Record, key string) (int64, bool) {
	raw, ok := record.values[key]
	if !ok {
		return 0, false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return 0, false
	}
	text, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(string(text), 10, 64)
	return n, err == nil
}

func recoveryAttempts(record *Record) (int64, bool) {
	if _, ok := record.values["resumedAfterInterruption"]; !ok {
		return 0, true
	}
	n, ok := recoveryInteger(record, "resumedAfterInterruption")
	return n, ok && n >= 0
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
