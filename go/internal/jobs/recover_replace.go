package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/lease"
)

const (
	asyncJobDefaultTimeoutSeconds = 24 * 60 * 60
	continuationPrompt            = "The previous execution was interrupted. Inspect the working tree and the task in this thread, avoid repeating completed steps, finish the remaining work, run the required focused validation, and report the final result."
	recoveryRetentionSeconds      = 3 * 24 * 60 * 60
)

func replaceRecoverJob(opt RecoverOptions, stateRoot, jobID string, job *Record, now float64) string {
	if opt.LaunchHost == nil {
		return "UNCONFIRMED"
	}
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	values := recordMap(job)
	queued := values["state"] == "QUEUED" && truthy(values["leaseQueued"])
	if queued && !truthy(values["hostPid"]) {
		generation, ok := recoveryInteger(job, "generation")
		if !ok {
			return "UNCONFIRMED"
		}
		token, ok := recoveryToken(opt)
		if !ok {
			return "UNCONFIRMED"
		}
		_ = job.Set("generation", generation+1)
		_ = job.Set("ownershipToken", token)
		if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
			return "UNCONFIRMED"
		}
		if opt.LaunchHost(dir, jobID, generation+1, token) != nil {
			return "UNCONFIRMED"
		}
		return "RECOVERED"
	}
	if queued {
		generation, ok := recoveryInteger(job, "generation")
		if !ok {
			return "UNCONFIRMED"
		}
		token, ok := recoveryToken(opt)
		if !ok {
			return "UNCONFIRMED"
		}
		for _, key := range []string{"hostPid", "hostCreatedAt", "hostContained", "signalPort"} {
			job.Delete(key)
		}
		_ = job.Set("generation", generation+1)
		_ = job.Set("ownershipToken", token)
		if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
			return "UNCONFIRMED"
		}
		if err := opt.LaunchHost(dir, jobID, generation+1, token); err != nil {
			return replacementHostLaunchFailed(job, dir, jobID, now, "Queued host launch failed")
		}
		return "RECOVERED"
	}
	return replaceInterruptedJob(opt, stateRoot, dir, jobID, job, now)
}

func replacementHostLaunchFailed(job *Record, dir, jobID string, now float64, message string) string {
	_ = job.Set("state", "UNKNOWN_OR_IN_PROGRESS")
	_ = job.Set("finishedAt", now)
	_ = job.Set("errorCode", "RECOVERY_HOST_LAUNCH_FAILED")
	_ = job.Set("message", message)
	_ = Write(filepath.Join(dir, jobID+".json"), job)
	return "AMBIGUOUS"
}

func replaceInterruptedJob(opt RecoverOptions, stateRoot, dir, jobID string, job *Record, now float64) string {
	values := recordMap(job)
	ambiguous := func(code, message string) string {
		_ = job.Set("state", "UNKNOWN_OR_IN_PROGRESS")
		_ = job.Set("finishedAt", now)
		_ = job.Set("errorCode", code)
		_ = job.Set("message", message)
		_ = Write(filepath.Join(dir, jobID+".json"), job)
		return "AMBIGUOUS"
	}
	launchPath := filepath.Join(dir, jobID+".launch")
	launchData, err := os.ReadFile(launchPath)
	if err != nil {
		return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: "+err.Error())
	}
	var launch map[string]any
	if err = json.Unmarshal(launchData, &launch); err != nil {
		return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: "+err.Error())
	}
	originalName, hasName := launch["originalName"]
	if !hasName {
		originalName = launch["name"]
	}
	name, nameOK := originalName.(string)
	originalRaw, hasArgs := launch["originalArgs"]
	if !hasArgs {
		originalRaw = launch["args"]
	}
	originalArgs, argsOK := originalRaw.(map[string]any)
	if !nameOK || (name != "codex" && name != "codex-reply") || !argsOK {
		return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: invalid launch arguments")
	}
	threadID, threadExists := values["threadId"]
	if threadExists && threadID != nil {
		if text, ok := threadID.(string); !ok || text == "" {
			return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: invalid thread identity")
		}
	}
	var replacementName string
	var replacementArgs map[string]any
	if threadID != nil && threadExists {
		config, ok := values["threadConfig"].(map[string]any)
		_, cwdOK := config["cwd"].(string)
		_, configArgsOK := config["config_args"].([]any)
		if !ok || !cwdOK || !configArgsOK {
			return ambiguous("RECOVERY_THREAD_CONFIG_MISSING", "Thread resume configuration is missing")
		}
		replacementName = "codex-reply"
		replacementArgs = map[string]any{"threadId": threadID, "prompt": continuationPrompt}
		record := map[string]any{"thread_id": threadID}
		for key, value := range config {
			record[key] = value
		}
		if err := SaveRecord(filepath.Join(stateRoot, "codex_exec_mcp", "threads"), record); err != nil {
			return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: "+err.Error())
		}
	} else {
		if name != "codex" {
			return ambiguous("RECOVERY_THREAD_MISSING", "Original reply thread identity is missing")
		}
		prompt, ok := originalArgs["prompt"].(string)
		if !ok {
			return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: original prompt missing")
		}
		replacementName = "codex"
		replacementArgs = make(map[string]any, len(originalArgs)+1)
		for key, value := range originalArgs {
			replacementArgs[key] = value
		}
		replacementArgs["prompt"] = prompt + "\n\n" + continuationPrompt
	}
	if _, ok := originalArgs["prompt"].(string); !ok {
		return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: original prompt missing")
	}
	timeout := float64(asyncJobDefaultTimeoutSeconds)
	if raw, exists := originalArgs["timeoutSeconds"]; exists {
		var valid bool
		switch n := raw.(type) {
		case json.Number:
			timeout, err = n.Float64()
			valid = err == nil
		case float64:
			timeout, valid = n, true
		case int:
			timeout, valid = float64(n), true
		case int64:
			timeout, valid = float64(n), true
		}
		if !valid || timeout <= 0 {
			return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery timeout is invalid")
		}
	}
	clock := opt.Clock
	if clock == nil {
		clock = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	}
	deadline := clock() + timeout
	if err := extendRecoveryOwnership(stateRoot, jobID, values, deadline, clock()); err != nil {
		return ambiguous("RECOVERY_OWNERSHIP_MISSING", "Async ownership unavailable: "+err.Error())
	}
	launch["name"], launch["args"] = replacementName, replacementArgs
	launch["originalName"], launch["originalArgs"] = name, originalArgs
	data, err := marshalPythonJSON(launch)
	if err != nil {
		return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: "+err.Error())
	}
	if err = atomicWrite(launchPath, data); err != nil {
		return ambiguous("RECOVERY_LAUNCH_MISSING", "Recovery launch data unavailable: "+err.Error())
	}
	generation, ok := recoveryInteger(job, "generation")
	if !ok {
		return ambiguous("RECOVERY_PROOF_MISSING", "Host or child-tree termination proof is missing")
	}
	attempts, _ := recoveryAttempts(job)
	token, ok := recoveryToken(opt)
	if !ok {
		return ambiguous("RECOVERY_HOST_LAUNCH_FAILED", "Replacement host launch failed: ownership token unavailable")
	}
	for _, key := range []string{"hostPid", "hostCreatedAt", "hostContained", "signalPort", "startedAt", "finishedAt", "exitCode", "errorCode", "message"} {
		job.Delete(key)
	}
	_ = job.Set("state", "QUEUED")
	_ = job.Set("generation", generation+1)
	_ = job.Set("ownershipToken", token)
	_ = job.Set("resumedAfterInterruption", attempts+1)
	_ = job.Set("deadlineAt", deadline)
	if stall, ok := values["stallDetectedAt"].(float64); ok {
		_ = job.Set("errorCode", "STALLED")
		_ = job.Set("stallDetectedAt", stall)
	} else if _, ok := values["stallDetectedAt"].(int); ok {
		_ = job.Set("errorCode", "STALLED")
	}
	if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
		return ambiguous("RECOVERY_HOST_LAUNCH_FAILED", "Replacement host launch failed: "+err.Error())
	}
	if err := opt.LaunchHost(dir, jobID, generation+1, token); err != nil {
		return ambiguous("RECOVERY_HOST_LAUNCH_FAILED", "Replacement host launch failed: "+err.Error())
	}
	return "RECOVERED"
}

func extendRecoveryOwnership(stateRoot, jobID string, job map[string]any, deadline, now float64) error {
	data, err := os.ReadFile(filepath.Join(stateRoot, "async_jobs", jobID+".json"))
	if err != nil {
		return nil
	}
	var decoded any
	if err = json.Unmarshal(data, &decoded); err != nil {
		return nil
	}
	index, ok := decoded.(map[string]any)
	if !ok {
		return nil
	}
	callKey, callOK := index["call_key"].(string)
	projectKey, projectOK := index["project_key"].(string)
	if index["jobId"] != jobID || index["call"] != job["call"] || !callOK || !projectOK {
		return fmt.Errorf("async index does not match job")
	}
	pendingData, err := os.ReadFile(filepath.Join(stateRoot, "pending", callKey+".json"))
	if err != nil {
		return err
	}
	var pending map[string]any
	if err = json.Unmarshal(pendingData, &pending); err != nil {
		return err
	}
	if pending["job_id"] != jobID || pending["async_call"] != job["call"] {
		return fmt.Errorf("pending call does not match job")
	}
	ownerValue, exists := pending["lease_owner"]
	if !exists {
		ownerValue = callKey
	}
	owner, ok := ownerValue.(string)
	if !ok {
		return fmt.Errorf("LEASE_OWNER_MISSING")
	}
	result := lease.New(filepath.Join(stateRoot, "projects", projectKey)).ExtendRetention(owner, deadline+recoveryRetentionSeconds, now)
	if result.Status != "OK" {
		return fmt.Errorf("%s", result.Code)
	}
	return nil
}

func recoveryToken(opt RecoverOptions) (string, bool) {
	if opt.NewToken != nil {
		value := opt.NewToken()
		return value, value != ""
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(token[:]), true
}
