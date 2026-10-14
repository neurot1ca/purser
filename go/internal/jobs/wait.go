package jobs

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"purser-hook/internal/waiter"
)

var waitTerminal = map[string]bool{
	"SUCCEEDED": true, "FAILED": true, "CANCELLED": true,
	"TIMED_OUT": true, "UNKNOWN_OR_IN_PROGRESS": true,
}

type WaitResult struct {
	Lines    []string
	ExitCode int
}

var waitSleep = time.Sleep

// Wait follows a durable job record until it reaches the same outcome reported
// by the Python waiter. SignalWait is bounded so replacement generations are
// noticed promptly even while an older endpoint is still open.
func Wait(stateRoot, jobID string) WaitResult {
	return WaitContext(context.Background(), stateRoot, jobID)
}

// WaitContext stops observation only; session cancellation never cancels a job.
func WaitContext(ctx context.Context, stateRoot, jobID string) WaitResult {
	started := time.Now()
	maxWaitSeconds := int64(9 * 60)
	if configured, err := strconv.ParseInt(os.Getenv("PURSER_WAIT_MAX_SECONDS"), 10, 64); err == nil && configured > 0 {
		maxWaitSeconds = configured
	}
	if !validWaitID(jobID) {
		return WaitResult{Lines: []string{"purser-wait " + jobID + " UNKNOWN_JOB"}, ExitCode: 2}
	}
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	if readJobWithRetry(dir, jobID) == nil {
		return WaitResult{Lines: []string{"purser-wait " + jobID + " UNKNOWN_JOB"}, ExitCode: 2}
	}
	recoveryAttempted := false
	for {
		if ctx.Err() != nil {
			return WaitResult{Lines: []string{"purser-wait " + jobID + " SESSION_ENDED"}, ExitCode: 0}
		}
		job := readJobWithRetry(dir, jobID)
		if job == nil {
			return reportWaitRecord(dir, jobID, nil)
		}
		values := recordMap(job)
		state, ok := values["state"].(string)
		if ok && waitTerminal[state] {
			return reportWaitRecord(dir, jobID, job)
		}
		if state == "RUNNING" && !recoveryAttempted {
			pid, pidOK := recoveryInteger(job, "hostPid")
			created, createdOK := recoveryInteger(job, "hostCreatedAt")
			if pidOK && pid > 0 && int64(int(pid)) == pid && createdOK &&
				(runtime.GOOS != "windows" || truthy(values["hostContained"])) && created >= 0 &&
				recoveryProcessState(int(pid), uint64(created)) == "DEAD" {
				recoveryAttempted = true
				_ = RecoverJob(stateRoot, jobID, float64(time.Now().UnixNano())/1e9, RecoverOptions{})
				continue
			}
		}
		if stalled, claimed := surfaceWaitStall(dir, jobID, values); claimed {
			return WaitResult{Lines: stallWaitLines(jobID, stalled), ExitCode: 4}
		}
		port, portOK := waitSignalPort(job)
		token, tokenOK := values["signalToken"].(string)
		deadline, deadlineOK := values["deadlineAt"].(float64)
		if !portOK || !tokenOK || token == "" || !isASCII(token) || !deadlineOK || math.IsNaN(deadline) || math.IsInf(deadline, 0) {
			return WaitResult{Lines: []string{"purser-wait " + jobID + " INVALID_JOB"}, ExitCode: 2}
		}
		remainingSeconds := deadline - float64(time.Now().UnixNano())/1e9
		if remainingSeconds <= 0 {
			return reportWaitRecord(dir, jobID, nil)
		}
		remainingWaitSeconds := float64(maxWaitSeconds) - time.Since(started).Seconds()
		if remainingWaitSeconds <= 0 {
			return WaitResult{Lines: []string{"purser-wait " + jobID + " STILL_WAITING"}, ExitCode: 5}
		}
		if port != 0 {
			wait := time.Duration(math.Min(1, math.Min(remainingSeconds, remainingWaitSeconds)) * float64(time.Second))
			_, _ = SignalWait("127.0.0.1:"+strconv.Itoa(port), jobID, token, wait)
		}
		remainingSeconds = deadline - float64(time.Now().UnixNano())/1e9
		remainingWaitSeconds = float64(maxWaitSeconds) - time.Since(started).Seconds()
		pause := math.Min(0.1, math.Min(remainingSeconds, remainingWaitSeconds))
		if pause > 0 {
			waitSleep(time.Duration(pause * float64(time.Second)))
		}
	}
}

func validWaitID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range id {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func readJobWithRetry(dir, id string) *Record {
	path := filepath.Join(dir, id+".json")
	until := time.Now().Add(time.Second)
	for {
		record, err := Read(path)
		if err == nil {
			var got string
			if record.Get("jobId", &got) && got == id {
				return record
			}
			return nil
		}
		if !waitPermissionError(err) || !time.Now().Before(until) {
			return nil
		}
		time.Sleep(min(20*time.Millisecond, time.Until(until)))
	}
}

func waitPermissionError(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.Errno(32)
}

func waitSignalPort(record *Record) (int, bool) {
	raw, exists := record.values["signalPort"]
	if !exists || string(raw) == "null" {
		return 0, true
	}
	var number jsonNumber
	if !record.Get("signalPort", &number) || !number.validInteger {
		return 0, false
	}
	port, err := strconv.Atoi(number.text)
	return port, err == nil && port >= 1 && port <= 65535
}

type jsonNumber struct {
	text         string
	validInteger bool
}

func (n *jsonNumber) UnmarshalJSON(data []byte) error {
	n.text = string(data)
	n.validInteger = n.text != "" && !strings.ContainsAny(n.text, ".eE")
	return nil
}

func isASCII(value string) bool {
	for i := range value {
		if value[i] > 0x7f {
			return false
		}
	}
	return true
}

func surfaceWaitStall(dir, id string, values map[string]any) (map[string]any, bool) {
	stall, valid := waitNumber(values["stallDetectedAt"])
	if values["state"] != "RUNNING" || values["errorCode"] != "STALLED" || !valid {
		return nil, false
	}
	lock, err := AcquireOwnership(dir, id)
	if err != nil {
		return nil, false
	}
	defer lock.Release()
	current := readJobWithRetry(dir, id)
	if current == nil {
		return nil, false
	}
	latest := recordMap(current)
	latestStall, latestValid := waitNumber(latest["stallDetectedAt"])
	observed, observedValid := waitNumber(latest["waiterObservedStallAt"])
	if latest["state"] != "RUNNING" || latest["errorCode"] != "STALLED" || !latestValid || latestStall != stall || observedValid && observed == stall {
		return nil, false
	}
	if current.Set("waiterObservedStallAt", latestStall) != nil || Write(filepath.Join(dir, id+".json"), current) != nil {
		return nil, false
	}
	return latest, true
}

func waitNumber(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok && !math.IsNaN(number) && !math.IsInf(number, 0)
}

func stallWaitLines(id string, job map[string]any) []string {
	lines := []string{"purser-wait " + id + " STALLED (worker still running)"}
	var lastActivity float64
	lastActivityValid := false
	if live, ok := job["liveness"].(map[string]any); ok {
		lastActivity, lastActivityValid = waitNumber(live["lastActivityAt"])
	}
	minutes := 0
	if lastActivityValid {
		minutes = max(0, int((float64(time.Now().UnixNano())/1e9-lastActivity)/60))
	}
	receipt := "> ⚓ **Purser has seen no worker activity for " + strconv.Itoa(minutes) + " minutes.** The job is still running; I'm checking its status."
	lines = append(lines, "Do not call codex_result yet. Restart "+waiter.Command(id)+" in the background (use the PowerShell tool on Windows); continue waiting. Tell the user in the user's language exactly one line, translated from: \""+receipt+"\" Do not show the jobId to the user.")
	return lines
}

func reportWaitRecord(dir, id string, job *Record) WaitResult {
	if job == nil {
		job = readJobWithRetry(dir, id)
	}
	if job == nil {
		return WaitResult{Lines: []string{"purser-wait " + id + " UNKNOWN_JOB"}, ExitCode: 3}
	}
	values := recordMap(job)
	state, ok := values["state"].(string)
	if !ok || state == "" {
		return WaitResult{Lines: []string{"purser-wait " + id + " INVALID_JOB"}, ExitCode: 3}
	}
	if waitTerminal[state] {
		observeWaitTerminal(dir, id, state)
	}
	code := 3
	if state == "SUCCEEDED" {
		code = 0
	} else if waitTerminal[state] {
		code = 1
	}
	return WaitResult{Lines: []string{"purser-wait " + id + " " + state}, ExitCode: code}
}

func observeWaitTerminal(dir, id, state string) {
	lock, err := AcquireOwnership(dir, id)
	if err != nil {
		return
	}
	defer lock.Release()
	current := readJobWithRetry(dir, id)
	if current == nil {
		return
	}
	var currentState string
	if !current.Get("state", &currentState) || currentState != state {
		return
	}
	if current.Set("waiterObservedTerminalAt", float64(time.Now().UnixNano())/1e9) != nil || current.Set("waiterObservedTerminalState", state) != nil {
		return
	}
	_ = Write(filepath.Join(dir, id+".json"), current)
}
