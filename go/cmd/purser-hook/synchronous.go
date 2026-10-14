package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"purser-hook/internal/jobs"
	"purser-hook/internal/proc"
)

var errSynchronousCancelled = errors.New("synchronous tool cancelled")

var supervisorPrefix = []string{"supervisor"}

func runSynchronous(args map[string]any, name string) (map[string]any, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runSynchronousContext(ctx, args, name)
}

func runSynchronousContext(ctx context.Context, args map[string]any, name string) (map[string]any, error) {
	if name != "codex" && name != "codex-reply" {
		return nil, errors.New("unknown tool: " + name)
	}
	agent := "codex"
	if value, exists := args["agent"]; exists {
		var ok bool
		agent, ok = value.(string)
		if !ok || (agent != "codex" && agent != "opencode" && agent != "agy") {
			return legacyError(nil, "agent must be codex, opencode or agy"), nil
		}
	} else if configured := os.Getenv("PURSER_AGENT"); configured != "" {
		agent = configured
		if agent != "codex" && agent != "opencode" && agent != "agy" {
			return legacyError(nil, "agent must be codex, opencode or agy"), nil
		}
	}
	prompt, ok := args["prompt"].(string)
	if !ok {
		return legacyError(nil, "prompt must be a string"), nil
	}
	var threadID string
	var cwd, model, sandbox string
	var configArgs []string
	var config any
	if name == "codex-reply" {
		threadID, _ = args["threadId"].(string)
		if threadID == "" {
			threadID, _ = args["conversationId"].(string)
		}
		if threadID == "" {
			return legacyError(nil, "threadId must be a nonempty string"), nil
		}
		record := jobs.LoadRecord(jobs.StateDir(), threadID)
		if record == nil {
			return legacyError(&threadID, "thread is unknown to this server; a fresh codex call is required"), nil
		}
		recordedAgent, _ := record["agent"].(string)
		if err := jobs.ValidateAgentResume(agent, recordedAgent, threadID, threadID); err != nil {
			return legacyError(&threadID, err.Error()), nil
		}
		reply := jobs.ReplyConfig(record)
		cwd, _ = reply["cwd"].(string)
		model, _ = reply["model"].(string)
		sandbox, _ = reply["sandbox"].(string)
		rawConfigArgs, _ := reply["config_args"].([]any)
		configArgs = stringSlice(rawConfigArgs)
		if worktree, _ := record["worktreePath"].(string); worktree != "" {
			if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
				result := legacyError(&threadID, "worktree is unavailable")
				result["errorCode"] = "WORKTREE_MISSING"
				return result, nil
			}
		}
	} else {
		cwdValue, exists := args["cwd"]
		if !exists {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				return legacyError(nil, err.Error()), nil
			}
		} else {
			value, valid := cwdValue.(string)
			if !valid || value == "" {
				return legacyError(nil, "cwd must be a nonempty string"), nil
			}
			cwd = value
		}
		var err error
		cwd, err = filepath.Abs(cwd)
		if err != nil {
			return legacyError(nil, err.Error()), nil
		}
		if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = resolved
		}
		if value, exists := args["model"]; exists && value != nil {
			var valid bool
			model, valid = value.(string)
			if !valid {
				return legacyError(nil, "model must be a string"), nil
			}
		}
		config = args["config"]
		if config == nil {
			config = map[string]any{}
		}
		var errFlatten error
		configArgs, errFlatten = jobs.FlattenConfig(config)
		if errFlatten != nil {
			return legacyError(nil, "config must be an object"), nil
		}
		for _, field := range []struct{ input, target string }{
			{"developer-instructions", "developer_instructions"},
			{"base-instructions", "base_instructions"},
			{"compact-prompt", "compact_prompt"},
		} {
			if value, exists := args[field.input]; exists {
				text, valid := value.(string)
				if !valid {
					return legacyError(nil, field.input+" must be a string"), nil
				}
				encoded, _ := json.Marshal(text)
				configArgs = append(configArgs, field.target+"="+string(encoded))
			}
		}
		if value, exists := args["approval-policy"]; exists {
			policy, valid := value.(string)
			if !valid || (policy != "on-request" && policy != "never") {
				return legacyError(nil, "invalid approval-policy"), nil
			}
			encoded, _ := json.Marshal(policy)
			configArgs = append(configArgs, "approval_policy="+string(encoded))
		}
		if value, exists := args["sandbox"]; exists && value != nil {
			sandbox, ok = value.(string)
			if !ok || (sandbox != "read-only" && sandbox != "workspace-write" && sandbox != "danger-full-access") {
				return legacyError(nil, "invalid sandbox"), nil
			}
		}
		if agent != "codex" {
			_, hasApproval := args["approval-policy"]
			_, hasDeveloper := args["developer-instructions"]
			_, hasBase := args["base-instructions"]
			_, hasCompact := args["compact-prompt"]
			if hasApproval || hasDeveloper || hasBase || hasCompact {
				return legacyError(nil, "unsupported OpenCode option"), nil
			}
		}
	}
	if cwd == "" {
		return legacyError(nil, "thread working directory is unavailable"), nil
	}
	if agent == "opencode" && sandbox != "" {
		return legacyError(nil, "OpenCode sandbox has no verified equivalent"), nil
	}
	if err := os.MkdirAll(jobs.StateDir(), 0o700); err != nil {
		return legacyError(nil, err.Error()), nil
	}
	temp, err := os.MkdirTemp(jobs.StateDir(), "codex-output-")
	if err != nil {
		return legacyError(nil, err.Error()), nil
	}
	defer os.RemoveAll(temp)
	outputPath := filepath.Join(temp, "last-message.txt")
	var commandArgs []string
	if agent == "codex" {
		launch := jobs.LaunchArgs{Name: name, Cwd: cwd, Model: model, ThreadID: threadID, OutputPath: outputPath}
		if name == "codex" {
			launch.Config, launch.Sandbox = config, sandbox
			launch.DeveloperInstructions = stringArg(args, "developer-instructions")
			launch.BaseInstructions = stringArg(args, "base-instructions")
			launch.CompactPrompt = stringArg(args, "compact-prompt")
			launch.ApprovalPolicy = stringArg(args, "approval-policy")
		}
		commandArgs, err = jobs.BuildCodexCommand(launch)
		if err == nil && name == "codex-reply" {
			commandArgs = insertConfigArgs(commandArgs, configArgs)
		} else if err == nil {
			configArgs = commandConfigArgs(commandArgs)
		}
	} else {
		var sandboxArg *string
		if sandbox != "" {
			sandboxArg = &sandbox
		}
		prefix, prefixErr := jobs.ResolveAgentPrefix(agent, nil)
		if prefixErr != nil {
			return legacyError(nil, prefixErr.Error()), nil
		}
		commandArgs, err = jobs.BuildAgentCommand(jobs.AgentCommandInput{
			Agent: agent, Prefix: prefix, Tool: name, CWD: cwd, Model: model,
			ConfigArgs: configArgs, Sandbox: sandboxArg, ThreadID: threadID, Prompt: prompt,
		})
	}
	if err != nil || len(commandArgs) == 0 {
		if err == nil {
			err = errors.New("invalid launch arguments")
		}
		return legacyError(nil, err.Error()), nil
	}
	command := proc.Hide(exec.Command(commandArgs[0], commandArgs[1:]...))
	command.Dir = cwd
	if agent == "codex" {
		command.Stdin = strings.NewReader(prompt)
	}
	var input, inputWriter *os.File
	if command.Stdin != nil {
		input, inputWriter, err = os.Pipe()
		if err != nil {
			return legacyError(nil, err.Error()), nil
		}
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return legacyError(nil, err.Error()), nil
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return legacyError(nil, err.Error()), nil
	}
	executable, err := os.Executable()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return legacyError(nil, err.Error()), nil
	}
	supervision, err := proc.StartSupervised(ctx, executable, supervisorPrefix, proc.Launch{
		Args: command.Args, Dir: cwd, Env: command.Env, Stdin: input,
		Stdout: stdoutWriter, Stderr: stderrWriter,
	})
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		if input != nil {
			_ = input.Close()
			_ = inputWriter.Close()
		}
		result := legacyError(nil, err.Error())
		result["typedEvidence"] = jobs.LaunchFailureEvidence(agent, nil, "", false, false, false, nil)
		return result, nil
	}
	if input != nil {
		_ = input.Close()
		go func() { _, _ = io.Copy(inputWriter, command.Stdin); _ = inputWriter.Close() }()
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	stream := jobs.NewEventStream(agent)
	var tail limitedTail
	stdoutHadOutput := false
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)
	go func() {
		_, readErr := io.Copy(eventStreamWriter{stream, &stdoutHadOutput}, stdout)
		stdoutDone <- readErr
	}()
	go func() {
		_, readErr := io.Copy(&tail, stderr)
		stderrDone <- readErr
	}()
	supervised := supervision.Result()
	if !supervised.Confirmed {
		_ = stdout.Close()
		_ = stderr.Close()
		return legacyError(optionalThread(threadID), "worker process-tree teardown was not confirmed"), nil
	}
	drainTimer := time.NewTimer(2 * time.Second)
	defer drainTimer.Stop()
	outDone, errDone := (<-chan error)(stdoutDone), (<-chan error)(stderrDone)
	for outDone != nil || errDone != nil {
		select {
		case <-outDone:
			outDone = nil
		case <-errDone:
			errDone = nil
		case <-drainTimer.C:
			_ = stdout.Close()
			_ = stderr.Close()
			return legacyError(optionalThread(threadID), "worker output pipes remained open after tree teardown"), nil
		}
	}
	_ = stdout.Close()
	_ = stderr.Close()
	if ctx.Err() != nil {
		return nil, errSynchronousCancelled
	}
	if !supervised.Confirmed || supervised.Err != nil {
		return legacyError(optionalThread(threadID), fmt.Sprintf("worker execution or process-tree teardown failed: %v", supervised.Err)), nil
	}
	exitCode := supervised.RootExit
	eventResult := stream.Result(false, exitCode, tail.String())
	if agent == "agy" && name == "codex-reply" && eventResult.ThreadID != threadID {
		eventResult.Failure = strings.TrimSpace(eventResult.Failure + "\nAntigravity did not resume the requested conversation; start a fresh call")
	}
	completed := eventResult.Status == "success"
	failed := eventResult.Failure != ""
	if exitCode != 0 || eventResult.ThreadID == "" || failed || ((agent == "opencode" || agent == "agy") && !completed) {
		detail := eventResult.Failure
		if detail == "" {
			if (agent == "opencode" || agent == "agy") && !completed {
				detail = "turn completion event missing"
			} else if tail.String() != "" {
				detail = tail.String()
			} else {
				detail = fmt.Sprintf("exit code %d", exitCode)
			}
		}
		failure := legacyError(optionalThread(eventResult.ThreadID), detail)
		outputHadData := false
		if info, statErr := os.Stat(outputPath); statErr == nil {
			outputHadData = info.Size() > 0
		}
		failure["typedEvidence"] = jobs.LaunchFailureEvidence(agent, stream.Events(), tail.String(), true,
			stdoutHadOutput, outputHadData, &exitCode)
		return failure, nil
	}
	content := eventResult.Final
	if agent == "opencode" {
		for _, event := range stream.Events() {
			if kind, _ := event["type"].(string); kind == "text" {
				part, _ := event["part"].(map[string]any)
				if final, ok := part["text"].(string); ok {
					content = final
				}
			}
		}
	}
	if agent == "agy" {
		for _, event := range stream.Events() {
			if final, ok := event["response"].(string); ok {
				content = final
			}
		}
	}
	if agent == "codex" {
		if raw, readErr := os.ReadFile(outputPath); readErr == nil && len(raw) > 0 {
			content = string(raw)
		}
	}
	if name == "codex" {
		recordedConfigArgs := configArgs
		if recordedConfigArgs == nil {
			recordedConfigArgs = []string{}
		}
		record := map[string]any{"thread_id": eventResult.ThreadID, "cwd": cwd,
			"model": nullable(model), "config_args": recordedConfigArgs, "sandbox": nullable(sandbox), "agent": agent}
		if err = jobs.SaveRecord(jobs.StateDir(), record); err != nil {
			return legacyError(nil, err.Error()), nil
		}
	}
	return legacySuccess(eventResult.ThreadID, content, eventResult.Usage, agent == "codex"), nil
}

func stringArg(args map[string]any, key string) *string {
	value, ok := args[key].(string)
	if !ok {
		return nil
	}
	return &value
}

func insertConfigArgs(command, configArgs []string) []string {
	if len(configArgs) == 0 {
		return command
	}
	index := -1
	for i, arg := range command {
		if arg == "--skip-git-repo-check" {
			index = i + 1
			break
		}
	}
	if index < 0 {
		return command
	}
	if index+1 < len(command) && command[index] == "-m" {
		index += 2
	}
	options := make([]string, 0, 2*len(configArgs))
	for _, arg := range configArgs {
		options = append(options, "-c", arg)
	}
	command = append(command, make([]string, len(options))...)
	copy(command[index+len(options):], command[index:len(command)-len(options)])
	copy(command[index:index+len(options)], options)
	return command
}

func commandConfigArgs(command []string) []string {
	var values []string
	for i := 0; i+1 < len(command); i++ {
		if command[i] == "-c" {
			values = append(values, command[i+1])
			i++
		}
	}
	return values
}

type eventStreamWriter struct {
	stream          *jobs.EventStream
	stdoutHadOutput *bool
}

func (w eventStreamWriter) Write(data []byte) (int, error) {
	if len(bytes.TrimSpace(data)) > 0 {
		*w.stdoutHadOutput = true
	}
	w.stream.Write(data)
	return len(data), nil
}

type limitedTail struct{ data []byte }

func (w *limitedTail) Write(data []byte) (int, error) {
	const limit = 4000
	if len(data) >= limit {
		w.data = append(w.data[:0], data[len(data)-limit:]...)
	} else if extra := len(w.data) + len(data) - limit; extra > 0 {
		copy(w.data, w.data[extra:])
		w.data = w.data[:len(w.data)-extra]
		w.data = append(w.data, data...)
	} else {
		w.data = append(w.data, data...)
	}
	return len(data), nil
}

func (w *limitedTail) String() string { return string(w.data) }

func stringSlice(values []any) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func optionalThread(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func legacySuccess(thread, text string, usage map[string]any, codex bool) map[string]any {
	value := map[string]any{"threadId": thread, "content": text}
	if codex && len(usage) > 0 {
		value["usage"] = usage
	}
	result := map[string]any{"content": []any{map[string]any{"type": "text", "text": pythonToolJSON(value, false)}},
		"isError": false, "structuredContent": value}
	return result
}

func legacyError(thread *string, message string) map[string]any {
	var id any
	if thread != nil {
		id = *thread
	}
	value := map[string]any{"threadId": id, "error": message}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": pythonToolJSON(value, true)}}, "isError": true}
}

func pythonToolJSON(value map[string]any, failed bool) string {
	keys := []string{"threadId", "content"}
	if failed {
		keys = []string{"threadId", "error"}
	} else if _, ok := value["usage"]; ok {
		keys = append(keys, "usage")
	}
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buffer.WriteByte(',')
		}
		name, _ := marshalPythonJSON(key)
		encoded, _ := marshalPythonJSON(value[key])
		buffer.Write(name)
		buffer.WriteByte(':')
		buffer.Write(encoded)
	}
	buffer.WriteByte('}')
	return pythonJSONSeparators(buffer.String())
}

func marshalPythonJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func pythonJSONSeparators(value string) string {
	var result strings.Builder
	quoted := false
	for i := 0; i < len(value); {
		if quoted && value[i] == '\\' && i+1 < len(value) {
			switch {
			case strings.HasPrefix(value[i:], `\u2028`):
				result.WriteRune('\u2028')
				i += 6
			case strings.HasPrefix(value[i:], `\u2029`):
				result.WriteRune('\u2029')
				i += 6
			case strings.HasPrefix(value[i:], `\u0008`):
				result.WriteString(`\b`)
				i += 6
			case strings.HasPrefix(value[i:], `\u000c`):
				result.WriteString(`\f`)
				i += 6
			default:
				result.WriteString(value[i : i+2])
				i += 2
			}
			continue
		}
		char := value[i]
		result.WriteByte(char)
		i++
		if char == '"' {
			quoted = !quoted
		} else if !quoted && (char == ',' || char == ':') {
			result.WriteByte(' ')
		}
	}
	return result.String()
}
