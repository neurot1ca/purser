package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"purser-hook/internal/ledger"
	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
	"purser-hook/internal/retention"
)

const hostUnavailableCode = "GO_HOST_EXECUTION_UNAVAILABLE"

func validJobID(value string) bool {
	for i := range value {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// HostOptions injects the child executable for parity tests and embedders.
type HostOptions struct {
	WorkerCommand func(LaunchArgs) ([]string, error)
	HostContained bool
	StateRoot     string
}

// RunHostGeneration owns and executes one queued agent generation.
func RunHostGeneration(dir, jobID string, generation int64, token string, options ...HostOptions) (map[string]any, error) {
	if len(jobID) != 64 || !validJobID(jobID) || generation < 1 || token == "" {
		return nil, errors.New("invalid host generation identity")
	}
	var opt HostOptions
	if len(options) > 0 {
		opt = options[0]
	}
	path := filepath.Join(dir, jobID+".json")
	job, err := Read(path)
	if err != nil {
		return nil, err
	}
	var worktreePath, state string
	var leaseQueued bool
	var recordedID, recordedToken string
	var recordedGeneration int64
	job.Get("jobId", &recordedID)
	job.Get("generation", &recordedGeneration)
	job.Get("ownershipToken", &recordedToken)
	job.Get("worktreePath", &worktreePath)
	job.Get("leaseQueued", &leaseQueued)
	job.Get("state", &state)
	if recordedID != jobID || recordedGeneration != generation || recordedToken != token || state != "QUEUED" {
		return map[string]any{"status": "stale"}, nil
	}
	launchPath := filepath.Join(dir, jobID+".launch")
	launchData, err := os.ReadFile(launchPath)
	if err != nil {
		return nil, err
	}
	var launch struct {
		Name     string          `json:"name"`
		Args     json.RawMessage `json:"args"`
		CodexBin any             `json:"codexBin"`
		Agent    string          `json:"agent"`
	}
	if err = json.Unmarshal(launchData, &launch); err != nil {
		return nil, err
	}
	var argsMap map[string]any
	if err = json.Unmarshal(launch.Args, &argsMap); err != nil || argsMap == nil {
		return nil, errors.New("launch args must be an object")
	}
	var args LaunchArgs
	if err = json.Unmarshal(launch.Args, &args); err != nil {
		return nil, err
	}
	var prompt string
	if value, ok := argsMap["prompt"].(string); ok {
		prompt = value
	}
	args.Name = launch.Name
	if args.ThreadID == "" {
		args.ThreadID, _ = argsMap["threadId"].(string)
	}
	args.OutputPath = filepath.Join(dir, jobID+".host-output")
	if args.Cwd == "" {
		job.Get("repoRoot", &args.Cwd)
		if args.Cwd == "" {
			args.Cwd, err = resolveRepositoryRoot()
			if err != nil {
				return finishHostError(path, jobID, generation, token, "WORKTREE_SNAPSHOT_FAILED", "Repository root could not be resolved for Codex launch")
			}
		}
	}
	repoRoot := ""
	if worktreePath == "" && argsMap["worktree"] != true {
		if root, rootErr := ledger.GitTopLevel(args.Cwd); rootErr == nil {
			repoRoot = root
		}
	}
	defer os.Remove(args.OutputPath)
	jobTempDir := ""
	workerAlive := false
	defer func() {
		if jobTempDir == "" || workerAlive {
			return
		}
		finished, readErr := Read(path)
		var finalState string
		if readErr == nil && finished.Get("state", &finalState) && terminalJobs[finalState] {
			_ = os.RemoveAll(jobTempDir)
		}
	}()
	agent := launch.Agent
	if agent == "" {
		agent = "codex"
	}
	if launch.Name != "codex" && launch.Name != "codex-reply" {
		return finishHostError(path, jobID, generation, token, "CODEX_FAILED", "Invalid host launch tool")
	}
	deadline, ok := numericDeadline(job)
	if !ok {
		return nil, errors.New("job deadline is invalid")
	}
	terminal := make(chan struct{})
	var cancelled atomic.Bool
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, int64(deadline*1e9)))
	defer cancel()
	signalToken := ""
	job.Get("signalToken", &signalToken)
	if signalToken == "" {
		return nil, errors.New("job signal token is unavailable")
	}
	signal, err := StartSignalHost(jobID, signalToken, terminal, func() bool {
		if cancelled.Swap(true) {
			return false
		}
		cancel()
		current, readErr := Read(path)
		if readErr == nil {
			var currentState string
			current.Get("state", &currentState)
			if currentState == "QUEUED" {
				_ = finishJob(dir, jobID, "CANCELLED", "CANCELLED", nil, "Codex cancelled", float64(time.Now().UnixNano())/1e9, hostFinishOptions(generation, token))
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	defer signal.Close()
	createdValue, createdOK := hostProcessIdentity()
	if !createdOK {
		return finishHostError(path, jobID, generation, token, "PROCESS_UNCONFIRMED", "Host process identity unavailable")
	}
	{
		lock, lockErr := AcquireOwnership(dir, jobID)
		if lockErr != nil {
			return nil, lockErr
		}
		job, lockErr = Read(path)
		if lockErr == nil {
			job.Get("jobId", &recordedID)
			job.Get("generation", &recordedGeneration)
			job.Get("ownershipToken", &recordedToken)
			job.Get("state", &state)
			if recordedID != jobID || recordedGeneration != generation || recordedToken != token || state != "QUEUED" {
				_ = lock.Release()
				return map[string]any{"status": "stale"}, nil
			}
			address, _ := signal.Addr().(*net.TCPAddr)
			for _, field := range []struct {
				key   string
				value any
			}{
				{"hostPid", os.Getpid()}, {"hostCreatedAt", createdValue},
				{"hostContained", opt.HostContained}, {"signalPort", address.Port},
			} {
				if lockErr = job.Set(field.key, field.value); lockErr != nil {
					break
				}
			}
			if lockErr == nil && repoRoot != "" {
				lockErr = job.Set("repoRoot", repoRoot)
			}
			if lockErr == nil {
				lockErr = Write(path, job)
			}
		}
		if releaseErr := lock.Release(); lockErr == nil {
			lockErr = releaseErr
		}
		if lockErr != nil {
			return nil, lockErr
		}
	}
	var childEnv []string
	if worktreePath != "" || argsMap["worktree"] == true {
		stateRoot := opt.StateRoot
		if stateRoot == "" {
			stateRoot = filepath.Dir(filepath.Dir(dir))
		}
		var repoRoot, relativeCWD string
		if worktreePath != "" {
			var recordedRoot string
			job.Get("repoRoot", &recordedRoot)
			repoRoot = recordedRoot
			if repoRoot == "" {
				repoRoot = worktreePath
			}
			if info, statErr := os.Stat(worktreePath); statErr != nil || !info.IsDir() {
				return finishHostError(path, jobID, generation, token, "WORKTREE_MISSING", "Job worktree is unavailable")
			}
			requested, _ := filepath.Abs(args.Cwd)
			root, _ := filepath.Abs(repoRoot)
			relativeCWD, _ = filepath.Rel(root, requested)
			if relativeCWD == ".." || strings.HasPrefix(relativeCWD, ".."+string(filepath.Separator)) {
				relativeCWD = ""
			}
			var pending bool
			job.Get("replySnapshotPending", &pending)
			if pending {
				var resumed bool
				job.Get("resumedAfterInterruption", &resumed)
				if resumed {
					return finishHostError(path, jobID, generation, token, "WORKTREE_SNAPSHOT_INTERRUPTED", "Worktree snapshot was interrupted")
				}
				var baseline any
				if job.Get("baselineSnapshot", &baseline) {
					_ = job.Set("snapshot", baseline)
				}
				job.Delete("replySnapshotPending")
				if err = Write(path, job); err != nil {
					return nil, err
				}
			}
		} else {
			requested, absErr := filepath.Abs(args.Cwd)
			if absErr != nil {
				return finishHostError(path, jobID, generation, token, "WORKTREE_SNAPSHOT_FAILED", "Worktree snapshot could not be created")
			}
			gitCommand := proc.Hide(exec.Command("git", "-C", requested, "rev-parse", "--show-toplevel"))
			proc.ConfigureNoWindow(gitCommand)
			rootOut, gitErr := gitCommand.Output()
			if gitErr != nil {
				return finishHostError(path, jobID, generation, token, "WORKTREE_SNAPSHOT_FAILED", "Worktree snapshot could not be created")
			}
			repoRoot = strings.TrimSpace(string(rootOut))
			relativeCWD, _ = filepath.Rel(repoRoot, requested)
			snapshot, createErr := ledger.CreateJobWorktree(repoRoot, jobID, filepath.Join(stateRoot, "wt"))
			if createErr != nil {
				return finishHostError(path, jobID, generation, token, "WORKTREE_SNAPSHOT_FAILED", "Worktree snapshot could not be created")
			}
			worktreePath = snapshot["path"]
			threadID := ""
			_ = job.Get("threadId", &threadID)
			attached, attachErr := retention.AttachWorktree(stateRoot, worktreePath, jobID, threadID)
			if attachErr != nil || !attached {
				return finishHostError(path, jobID, generation, token, "WORKTREE_RETIRING", "Thread worktree is being retired")
			}
			job, err = Read(path)
			if err != nil {
				return nil, err
			}
			setup := profile.LoadProfile(repoRoot).Worktree
			setupResult := ledger.RunWorktreeSetup(worktreePath, setup)
			if setupResult.Failed() {
				if err = job.Set("worktreePath", worktreePath); err != nil {
					return nil, err
				}
				if err = job.Set("repoRoot", repoRoot); err != nil {
					return nil, err
				}
				_ = job.Set("setupError", setupResult.Failure)
				if err = Write(path, job); err != nil {
					return nil, err
				}
				return finishHostError(path, jobID, generation, token, "WORKTREE_SETUP_FAILED", "Worktree setup failed")
			}
			for key, value := range map[string]any{"worktreePath": snapshot["path"], "snapshot": snapshot["snapshot"], "baselineSnapshot": snapshot["snapshot"], "head": snapshot["head"], "repoRoot": repoRoot} {
				if err = job.Set(key, value); err != nil {
					return nil, err
				}
			}
			resumePartialAttempt(dir, job, worktreePath, &prompt)
			if err = Write(path, job); err != nil {
				return nil, err
			}
		}
		mapped := filepath.Join(worktreePath, relativeCWD)
		if info, statErr := os.Stat(mapped); statErr != nil || !info.IsDir() {
			_ = job.Set("cwdFallback", relativeCWD)
			mapped = worktreePath
			if err = Write(path, job); err != nil {
				return nil, err
			}
		}
		args.Cwd = mapped
	}
	if agent == "codex" {
		stateRoot := opt.StateRoot
		if stateRoot == "" {
			stateRoot = filepath.Dir(filepath.Dir(dir))
		}
		cacheUser, cacheErr := acquireCacheUser(stateRoot)
		if cacheErr != nil {
			return finishHostError(path, jobID, generation, token, "CODEX_FAILED", "Codex Go cache is busy")
		}
		defer cacheUser()
		jobTempDir = filepath.Join(stateRoot, "cache", "tmp", jobID)
		var writableRoots []any
		if worktreePath != "" {
			writableRoots = append(writableRoots, worktreePath)
		}
		if err = configureCodexGoCache(&args, &childEnv, stateRoot, jobTempDir, writableRoots); err != nil {
			return finishHostError(path, jobID, generation, token, "CODEX_FAILED", "Codex Go cache directory is unavailable")
		}
	}
	if launch.CodexBin != nil {
		encoded, _ := json.Marshal(launch.CodexBin)
		args.BinaryOverride = string(encoded)
	}
	var commandArgs []string
	if agent == "codex" {
		commandArgs, err = BuildCodexCommand(args)
	} else {
		configArgs, configErr := FlattenConfig(args.Config)
		if configErr != nil {
			err = configErr
		} else {
			var prefix []string
			if launch.CodexBin != nil {
				prefix, err = ResolveAgentPrefix(agent, launch.CodexBin)
			} else {
				prefix, err = ResolveAgentPrefix(agent, nil)
			}
			if err == nil {
				var sandbox *string
				if args.Sandbox != "" {
					sandbox = &args.Sandbox
				}
				commandArgs, err = BuildAgentCommand(AgentCommandInput{Agent: agent, Prefix: prefix, Tool: launch.Name, CWD: args.Cwd, Model: args.Model, ConfigArgs: configArgs, Sandbox: sandbox, ThreadID: args.ThreadID, Prompt: prompt})
			}
		}
	}
	if opt.WorkerCommand != nil {
		commandArgs, err = opt.WorkerCommand(args)
	}
	if err != nil || len(commandArgs) == 0 {
		return finishHostError(path, jobID, generation, token, "CODEX_FAILED", "Codex launch arguments are invalid")
	}
	if leaseQueued {
		root := filepath.Dir(filepath.Dir(dir))
		if err = WaitForQueuedBaseline(ctx, root, jobID, time.Second); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				_ = finishJob(dir, jobID, "TIMED_OUT", "DEADLINE_EXCEEDED", nil, "Codex deadline exceeded", float64(time.Now().UnixNano())/1e9, hostFinishOptions(generation, token))
				return map[string]any{"status": "ok"}, nil
			}
			if errors.Is(err, context.Canceled) {
				_ = finishJob(dir, jobID, "CANCELLED", "CANCELLED", nil, "Codex cancelled", float64(time.Now().UnixNano())/1e9, hostFinishOptions(generation, token))
				return map[string]any{"status": "ok"}, nil
			}
			message := "Fresh admission baseline could not be bound; FULL review required: " + err.Error()
			_ = finishJob(dir, jobID, "FAILED", "ADMISSION_BASELINE_UNAVAILABLE", nil, message, float64(time.Now().UnixNano())/1e9, hostFinishOptions(generation, token))
			return map[string]any{"status": "error", "errorCode": "ADMISSION_BASELINE_UNAVAILABLE", "message": message}, nil
		}
	}
	if cancelled.Load() {
		return map[string]any{"status": "ok"}, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || float64(time.Now().UnixNano())/1e9 >= deadline {
		_ = finishJob(dir, jobID, "TIMED_OUT", "DEADLINE_EXCEEDED", nil, "Codex deadline exceeded", float64(time.Now().UnixNano())/1e9, hostFinishOptions(generation, token))
		return map[string]any{"status": "ok"}, nil
	}
	if ctx.Err() != nil {
		return map[string]any{"status": "ok"}, nil
	}
	now := float64(time.Now().UnixNano()) / 1e9
	lock, err := AcquireOwnership(dir, jobID)
	if err != nil {
		return nil, err
	}
	job, err = Read(path)
	if err == nil {
		job.Get("jobId", &recordedID)
		job.Get("generation", &recordedGeneration)
		job.Get("ownershipToken", &recordedToken)
		job.Get("state", &state)
		if recordedID != jobID || recordedGeneration != generation || recordedToken != token || state != "QUEUED" {
			_ = lock.Release()
			return map[string]any{"status": "stale"}, nil
		}
		now = float64(time.Now().UnixNano()) / 1e9
		_ = job.Set("state", "RUNNING")
		_ = job.Set("startedAt", now)
		err = Write(path, job)
	}
	if releaseErr := lock.Release(); err == nil {
		err = releaseErr
	}
	if err != nil {
		return nil, err
	}
	var result EventResult
	var failureEvidence map[string]any
	for {
		command := proc.Hide(exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...))
		if len(childEnv) != 0 {
			command.Env = os.Environ()
			for _, override := range childEnv {
				key, _, _ := strings.Cut(override, "=")
				found := false
				for index, entry := range command.Env {
					entryKey, _, _ := strings.Cut(entry, "=")
					if strings.EqualFold(entryKey, key) {
						command.Env[index] = override
						found = true
					}
				}
				if !found {
					command.Env = append(command.Env, override)
				}
			}
		}
		command.Dir = args.Cwd
		command.Stdin = strings.NewReader(prompt)
		stream := NewEventStream(agent)
		activity := NewLivenessActivity(job, agent, StallThreshold(dir, job), now)
		activityMu := &sync.Mutex{}
		command.Stdout = eventWriter{stream: stream, activity: activity, activityMu: activityMu}
		stderr := &tailBuffer{limit: 4000}
		command.Stderr = stderr
		proc.ConfigureProcessTreeKill(command)
		lock, err = AcquireOwnership(dir, jobID)
		if err != nil {
			return nil, err
		}
		job, err = Read(path)
		if err == nil {
			job.Get("generation", &recordedGeneration)
			job.Get("ownershipToken", &recordedToken)
			job.Get("state", &state)
			if recordedGeneration != generation || recordedToken != token || state != "RUNNING" {
				_ = lock.Release()
				return map[string]any{"status": "stale"}, nil
			}
			err = command.Start()
			if err != nil {
				_ = lock.Release()
				close(terminal)
				contextErr := ctx.Err()
				if contextErr == nil && float64(time.Now().UnixNano())/1e9 >= deadline {
					contextErr = context.DeadlineExceeded
				}
				return finishHostStartError(dir, jobID, generation, token, cancelled.Load(), contextErr, err)
			}
			workerAlive = true
			_ = job.Set("pid", command.Process.Pid)
			err = Write(path, job)
		}
		if releaseErr := lock.Release(); err == nil {
			err = releaseErr
		}
		if err != nil {
			if command.Process != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				workerAlive = false
			}
			return nil, err
		}
		activityMu.Lock()
		_, checkpointErr := CheckpointLiveness(path, generation, token, activity, float64(time.Now().UnixNano())/1e9, true, true)
		activityMu.Unlock()
		if checkpointErr != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			workerAlive = false
			return nil, checkpointErr
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		checkTicker := time.NewTicker(100 * time.Millisecond)
		defer checkTicker.Stop()
		var runErr error
		for running := true; running; {
			select {
			case runErr = <-done:
				running = false
			case <-checkTicker.C:
				activityMu.Lock()
				_, checkpointErr := CheckpointLiveness(path, generation, token, activity, float64(time.Now().UnixNano())/1e9, true, false)
				activityMu.Unlock()
				if checkpointErr != nil {
					_ = command.Process.Kill()
					<-done
					workerAlive = false
					return nil, checkpointErr
				}
			}
		}
		workerAlive = false
		checkTicker.Stop()
		if !cancelled.Load() {
			activityMu.Lock()
			lastActivity := numberOr(activity["lastActivityAt"], float64(time.Now().UnixNano())/1e9)
			activity["longestSilentGapSeconds"] = math.Max(numberOr(activity["longestSilentGapSeconds"], 0), math.Max(0, float64(time.Now().UnixNano())/1e9-lastActivity))
			_, checkpointErr = CheckpointLiveness(path, generation, token, activity, float64(time.Now().UnixNano())/1e9, false, true)
			activityMu.Unlock()
			if checkpointErr != nil {
				return nil, checkpointErr
			}
		}
		exitCode := 0
		if runErr != nil {
			var exit *exec.ExitError
			if errors.As(runErr, &exit) {
				exitCode = exit.ExitCode()
			} else {
				exitCode = 1
			}
		}
		if cancelled.Load() || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			exitCode = 1
		}
		lock, err = AcquireOwnership(dir, jobID)
		if err == nil {
			job, err = Read(path)
			if err == nil {
				_ = job.Set("exitCode", exitCode)
				events := stream.Events()
				if evidence := TestEvidence(events); len(evidence) > 0 {
					_ = job.Set("testEvidence", evidence)
				}
				_ = job.Set("testEvidenceFailed", TestEvidenceFailed(events))
				err = Write(path, job)
			}
			if releaseErr := lock.Release(); err == nil {
				err = releaseErr
			}
		}
		if err != nil {
			return nil, err
		}
		result = stream.Result(cancelled.Load() || ctx.Err() != nil, exitCode, stderr.String())
		if cancelled.Load() || ctx.Err() != nil {
			break
		}
		failureEvidence = LaunchFailureEvidence(agent, stream.Events(), stderr.String(), true,
			numberOr(activity["stdoutBytes"], 0) > 0, hostOutputHasData(args.OutputPath), &exitCode)
		if err = saveHostAttemptFields(path, jobID, generation, token, result.Status != "success" || result.ThreadID == "", failureEvidence); err != nil {
			return nil, err
		}
		if result.Status == "success" {
			break
		}
		delay, retry, retryErr := reserveCapacityRetry(path, jobID, generation, token, failureEvidence)
		if retryErr != nil {
			return nil, retryErr
		}
		if !retry {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if cancelled.Load() || ctx.Err() != nil {
			break
		}
	}
	if result.ThreadID != "" {
		configArgs, configErr := FlattenConfig(args.Config)
		if configErr != nil {
			return finishHostError(path, jobID, generation, token, "CODEX_FAILED", "Codex launch configuration is invalid")
		}
		if configArgs == nil {
			configArgs = []string{}
		}
		var model, sandbox any
		if args.Model != "" {
			model = args.Model
		}
		if args.Sandbox != "" {
			sandbox = args.Sandbox
		}
		threadConfig := map[string]any{"cwd": args.Cwd, "model": model, "config_args": configArgs,
			"sandbox": sandbox, "agent": agent}
		for source, target := range map[string]string{"taskId": "task_id", "route": "route", "model": "model", "effort": "effort"} {
			var value any
			if job.Get(source, &value) && value != nil {
				threadConfig[target] = value
			}
		}
		for _, key := range []string{"worktreePath", "snapshot", "baselineSnapshot", "head", "repoRoot"} {
			var value any
			if job.Get(key, &value) {
				threadConfig[key] = value
			}
		}
		if err = saveHostThread(dir, jobID, generation, token, result.ThreadID, threadConfig, launch.Name == "codex", opt.StateRoot); err != nil {
			return nil, err
		}
	}
	finished := float64(time.Now().UnixNano()) / 1e9
	switch {
	case cancelled.Load():
		err = finishJob(dir, jobID, "CANCELLED", "CANCELLED", nil, "Codex cancelled", finished, hostFinishOptions(generation, token))
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = finishJob(dir, jobID, "TIMED_OUT", "DEADLINE_EXCEEDED", nil, "Codex deadline exceeded", finished, hostFinishOptions(generation, token))
	case result.Status != "success" || result.ThreadID == "":
		message := result.Failure
		if message == "" {
			message = "Codex completion event missing"
		}
		message = "Codex failed: " + message
		code := "CODEX_FAILED"
		if attempts, valid := recoveryInteger(job, "capacityRetryAttempts"); valid && attempts >= 2 &&
			failureEvidence["type"] == "RATE_LIMITED" && failureEvidence["noWorkProof"] == true {
			code = "CAPACITY_RETRIES_EXHAUSTED"
			message += "; automatic capacity retries exhausted"
		}
		err = finishJob(dir, jobID, "FAILED", code, nil, message, finished, hostFinishOptions(generation, token))
	default:
		content := result.Final
		if data, readErr := os.ReadFile(args.OutputPath); readErr == nil {
			content = string(data)
		}
		jobResult := map[string]any{
			"content": content, "threadId": result.ThreadID, "usage": result.Usage,
		}
		if len(result.Notes) > 0 {
			jobResult["notes"] = result.Notes
		}
		checkReadinessBeforeSuccess(dir, jobID, jobResult)
		finished = float64(time.Now().UnixNano()) / 1e9
		err = finishJob(dir, jobID, "SUCCEEDED", "", jobResult, "", finished, hostFinishOptions(generation, token))
	}
	close(terminal)
	time.Sleep(200 * time.Millisecond)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "ok"}, nil
}

func configureCodexGoCache(args *LaunchArgs, childEnv *[]string, stateRoot, jobTempDir string, writableRoots []any) error {
	config := map[string]any{}
	switch existing := args.Config.(type) {
	case OrderedObject:
		for _, entry := range existing {
			config[entry.Key] = entry.Value
		}
	case map[string]any:
		config = existing
	}
	workspaceValue := config["sandbox_workspace_write"]
	var existingRoots any
	switch workspace := workspaceValue.(type) {
	case map[string]any:
		existingRoots = workspace["writable_roots"]
	case []ConfigEntry:
		for _, entry := range workspace {
			if entry.Key == "writable_roots" {
				existingRoots = entry.Value
				break
			}
		}
	case OrderedObject:
		for _, entry := range workspace {
			if entry.Key == "writable_roots" {
				existingRoots = entry.Value
				break
			}
		}
	}
	if existing, ok := existingRoots.([]any); ok {
		writableRoots = append(writableRoots, existing...)
	}
	if configDir, err := os.UserConfigDir(); err == nil {
		telemetryDir := filepath.Join(configDir, "go", "telemetry")
		if info, err := os.Stat(telemetryDir); err == nil && info.IsDir() {
			writableRoots = append(writableRoots, telemetryDir)
		}
	}
	if err := os.MkdirAll(jobTempDir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"TEMP", "TMP", "TMPDIR"} {
		*childEnv = append(*childEnv, name+"="+jobTempDir)
	}
	for _, variable := range []struct{ name, directory string }{
		{"GOCACHE", filepath.Join(stateRoot, "cache", "go-build")},
	} {
		if os.Getenv(variable.name) != "" {
			continue
		}
		if err := os.MkdirAll(variable.directory, 0700); err != nil {
			return err
		}
		writableRoots = append(writableRoots, variable.directory)
		*childEnv = append(*childEnv, variable.name+"="+variable.directory)
	}
	writableRoots = append(writableRoots, jobTempDir)
	if os.Getenv("GOTMPDIR") == "" {
		*childEnv = append(*childEnv, "GOTMPDIR="+jobTempDir)
	}
	for key := range config {
		if strings.HasPrefix(key, "sandbox_workspace_write.") {
			delete(config, key)
		}
	}
	if len(writableRoots) > 0 {
		config["sandbox_workspace_write"] = map[string]any{"writable_roots": writableRoots}
	}
	args.Config = config
	return nil
}

func finishHostStartError(dir, id string, generation int64, token string, cancelled bool, contextErr, startErr error) (map[string]any, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	switch {
	case cancelled:
		if err := finishJob(dir, id, "CANCELLED", "CANCELLED", nil, "Codex cancelled", now, hostFinishOptions(generation, token)); err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok"}, nil
	case errors.Is(contextErr, context.DeadlineExceeded):
		if err := finishJob(dir, id, "TIMED_OUT", "DEADLINE_EXCEEDED", nil, "Codex deadline exceeded", now, hostFinishOptions(generation, token)); err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok"}, nil
	default:
		if err := finishJob(dir, id, "FAILED", "CODEX_FAILED", nil, startErr.Error(), now, hostFinishOptions(generation, token)); err != nil {
			return nil, err
		}
		return map[string]any{"status": "error", "errorCode": "CODEX_FAILED", "message": startErr.Error()}, nil
	}
}

func saveHostAttemptFields(path, id string, generation int64, token string, failed bool, evidence map[string]any) error {
	lock, err := AcquireOwnership(filepath.Dir(path), id)
	if err != nil {
		return err
	}
	job, err := Read(path)
	if err == nil {
		var actual int64
		var owner, state string
		job.Get("generation", &actual)
		job.Get("ownershipToken", &owner)
		job.Get("state", &state)
		if actual == generation && owner == token && state == "RUNNING" {
			attempts, _ := recoveryInteger(job, "capacityRetryAttempts")
			_ = job.Set("responsivenessAttempts", attempts+1)
			if failed {
				_ = job.Set("launchFailureEvidence", evidence)
			} else {
				job.Delete("launchFailureEvidence")
			}
			err = Write(path, job)
		}
	}
	if releaseErr := lock.Release(); err == nil {
		err = releaseErr
	}
	return err
}

func hostOutputHasData(path string) bool {
	data, err := os.ReadFile(path)
	return len(data) > 0 || err != nil && !errors.Is(err, os.ErrNotExist)
}

type eventWriter struct {
	stream     *EventStream
	activity   LivenessActivity
	activityMu *sync.Mutex
}

func (w eventWriter) Write(value []byte) (int, error) {
	w.stream.Write(value)
	if w.activityMu != nil {
		w.activityMu.Lock()
		w.activity["stdoutBytes"] = int(numberOr(w.activity["stdoutBytes"], 0)) + len(value)
		RecordLivenessActivity(w.activity, float64(time.Now().UnixNano())/1e9)
		w.activityMu.Unlock()
	}
	return len(value), nil
}

type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	b.data = append(b.data, value...)
	if len(b.data) > b.limit {
		b.data = append(b.data[:0], b.data[len(b.data)-b.limit:]...)
	}
	return len(value), nil
}

func (b *tailBuffer) String() string { return string(b.data) }

func numericDeadline(job *Record) (float64, bool) {
	var value float64
	if !job.Get("deadlineAt", &value) || value <= 0 {
		return 0, false
	}
	return value, true
}

func hostFinishOptions(generation int64, token string) FinishOptions {
	return FinishOptions{Generation: &generation, Token: token}
}

func saveHostThread(dir, id string, generation int64, token, threadID string, config map[string]any, saveRecord bool, stateRoot string) error {
	lock, err := AcquireOwnership(dir, id)
	if err != nil {
		return err
	}
	owned := false
	job, err := Read(filepath.Join(dir, id+".json"))
	if err == nil {
		var actual int64
		var owner, state string
		job.Get("generation", &actual)
		job.Get("ownershipToken", &owner)
		job.Get("state", &state)
		if actual == generation && owner == token && state == "RUNNING" {
			owned = true
			err = job.Set("threadId", threadID)
			if err == nil {
				err = job.Set("threadConfig", config)
			}
			if err == nil {
				err = Write(filepath.Join(dir, id+".json"), job)
			}
		}
	}
	if releaseErr := lock.Release(); err == nil {
		err = releaseErr
	}
	if err != nil || !owned || !saveRecord {
		return err
	}
	if worktree := nonemptyString(config["worktreePath"]); worktree != "" {
		if stateRoot == "" {
			stateRoot = filepath.Dir(filepath.Dir(dir))
		}
		attached, attachErr := retention.AttachWorktree(stateRoot, worktree, id, threadID)
		if attachErr != nil || !attached {
			return fmt.Errorf("thread worktree attachment was rejected")
		}
	}
	record := map[string]any{"thread_id": threadID}
	for key, value := range config {
		record[key] = value
	}
	return SaveRecord(filepath.Join(filepath.Dir(filepath.Dir(dir)), "codex_exec_mcp", "threads"), record)
}

func failHostUnavailable(path, id string, generation int64, token, message string) (map[string]any, error) {
	lock, err := AcquireOwnership(filepath.Dir(path), id)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	job, err := Read(path)
	if err != nil {
		return nil, err
	}
	var actual int64
	var owner, state string
	job.Get("generation", &actual)
	job.Get("ownershipToken", &owner)
	job.Get("state", &state)
	if actual != generation || owner != token || state != "QUEUED" {
		return map[string]any{"status": "stale"}, nil
	}
	_ = job.Set("state", "FAILED")
	_ = job.Set("finishedAt", float64(time.Now().UnixNano())/1e9)
	_ = job.Set("errorCode", hostUnavailableCode)
	_ = job.Set("message", message)
	var displayCount any
	if !job.Get("boardDisplayCount", &displayCount) {
		_ = job.Set("boardDisplayCount", 0)
	}
	if err = Write(path, job); err != nil {
		return nil, err
	}
	finishFailedAttempt(filepath.Dir(path), id, job)
	_ = os.Remove(filepath.Join(filepath.Dir(path), id+".launch"))
	return map[string]any{"status": "error", "errorCode": hostUnavailableCode, "message": message}, nil
}

func finishHostError(path, id string, generation int64, token, code, message string) (map[string]any, error) {
	err := finishJob(filepath.Dir(path), id, "FAILED", code, nil, message,
		float64(time.Now().UnixNano())/1e9, hostFinishOptions(generation, token))
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "error", "errorCode": code, "message": message}, nil
}
