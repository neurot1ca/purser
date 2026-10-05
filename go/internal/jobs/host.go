package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"purser-hook/internal/proc"
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
}

// RunHostGeneration owns and executes one queued Codex generation. Worktree,
// queued-lease, and alternate-agent execution remain explicitly unavailable.
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
	if worktreePath != "" || argsMap["worktree"] == true || leaseQueued ||
		(launch.Agent != "" && launch.Agent != "codex") ||
		(launch.Name != "codex" && launch.Name != "codex-reply") {
		return failHostUnavailable(path, jobID, generation, token, "Go host branch is not ported")
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
	args.OutputPath = filepath.Join(dir, jobID+".host-output")
	defer os.Remove(args.OutputPath)
	if launch.CodexBin != nil {
		encoded, _ := json.Marshal(launch.CodexBin)
		args.BinaryOverride = string(encoded)
	}
	commandArgs, err := BuildCodexCommand(args)
	if opt.WorkerCommand != nil {
		commandArgs, err = opt.WorkerCommand(args)
	}
	if err != nil || len(commandArgs) == 0 {
		return finishHostError(path, jobID, generation, token, "CODEX_FAILED", "Codex launch arguments are invalid")
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
		if cancel != nil {
			cancel()
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
	lock, err := AcquireOwnership(dir, jobID)
	if err != nil {
		return nil, err
	}
	job, err = Read(path)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	job.Get("jobId", &recordedID)
	job.Get("generation", &recordedGeneration)
	job.Get("ownershipToken", &recordedToken)
	job.Get("state", &state)
	if recordedID != jobID || recordedGeneration != generation || recordedToken != token || state != "QUEUED" {
		_ = lock.Release()
		return map[string]any{"status": "stale"}, nil
	}
	now := float64(time.Now().UnixNano()) / 1e9
	address, _ := signal.Addr().(*net.TCPAddr)
	for _, field := range []struct {
		key   string
		value any
	}{
		{"state", "RUNNING"}, {"hostPid", os.Getpid()}, {"hostCreatedAt", createdValue},
		{"hostContained", opt.HostContained}, {"signalPort", address.Port}, {"startedAt", now},
	} {
		if err = job.Set(field.key, field.value); err != nil {
			break
		}
	}
	if err == nil {
		err = Write(path, job)
	}
	if releaseErr := lock.Release(); err == nil {
		err = releaseErr
	}
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	command.Dir = args.Cwd
	command.Stdin = strings.NewReader(prompt)
	stream := NewEventStream("codex")
	command.Stdout = eventWriter{stream}
	stderr := &tailBuffer{limit: 4000}
	command.Stderr = stderr
	proc.ConfigureProcessTreeKill(command)
	runErr := command.Run()
	if runErr != nil && command.Process == nil {
		close(terminal)
		_ = finishJob(dir, jobID, "FAILED", "CODEX_FAILED", nil, runErr.Error(), float64(time.Now().UnixNano())/1e9, hostFinishOptions(generation, token))
		return map[string]any{"status": "error", "errorCode": "CODEX_FAILED", "message": runErr.Error()}, nil
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
	result := stream.Result(cancelled.Load() || ctx.Err() != nil, exitCode, stderr.String())
	if result.ThreadID != "" {
		configArgs, configErr := FlattenConfig(args.Config)
		if configErr != nil {
			return finishHostError(path, jobID, generation, token, "CODEX_FAILED", "Codex launch configuration is invalid")
		}
		if configArgs == nil {
			configArgs = []string{}
		}
		threadConfig := map[string]any{"cwd": args.Cwd, "model": args.Model, "config_args": configArgs,
			"sandbox": args.Sandbox, "agent": "codex"}
		if err = saveHostThread(dir, jobID, generation, token, result.ThreadID, threadConfig, launch.Name == "codex"); err != nil {
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
		err = finishJob(dir, jobID, "FAILED", "CODEX_FAILED", nil, message, finished, hostFinishOptions(generation, token))
	default:
		content := result.Final
		if data, readErr := os.ReadFile(args.OutputPath); readErr == nil {
			content = string(data)
		}
		err = finishJob(dir, jobID, "SUCCEEDED", "", map[string]any{
			"content": content, "threadId": result.ThreadID, "usage": result.Usage,
		}, "", finished, hostFinishOptions(generation, token))
	}
	close(terminal)
	time.Sleep(200 * time.Millisecond)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "ok"}, nil
}

type eventWriter struct{ stream *EventStream }

func (w eventWriter) Write(value []byte) (int, error) {
	w.stream.Write(value)
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

func saveHostThread(dir, id string, generation int64, token, threadID string, config map[string]any, saveRecord bool) error {
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
