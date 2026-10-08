package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"purser-hook/internal/enforce"
	"purser-hook/internal/jobs"
	"purser-hook/internal/obsidian"
	"purser-hook/internal/policy"
)

func privateMain() (code int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			privateError(fmt.Errorf("internal panic: %v", recovered))
			code = 1
		}
	}()
	if os.Args[1] == "host" {
		return hostMain(os.Args[2:])
	}
	return operationMain(os.Args[2:])
}

func operationMain(argv []string) int {
	if len(argv) != 1 {
		privateError(errors.New("usage: purser-hook op <codex_start|codex_result|codex_cancel|codex_apply>"))
		return 2
	}
	args, err := readOperationArgs()
	if err != nil {
		privateError(err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := dispatchOperation(ctx, argv[0], args, stateRoot())
	if err != nil {
		if errors.Is(err, errSynchronousCancelled) {
			return 0
		}
		privateError(err)
		var inputErr operationInputError
		if errors.As(err, &inputErr) {
			return 2
		}
		return 1
	}
	status := "ok"
	if result == nil {
		privateError(errors.New("operation returned no result"))
		return 1
	}
	structured, _ := result["structuredContent"].(map[string]any)
	if result["isError"] == true || structured["errorCode"] != nil {
		status = "error"
	}
	writePrivateJSON(map[string]any{"status": status, "result": result})
	return 0
}

type operationInputError struct{ message string }

func (e operationInputError) Error() string { return e.message }

func dispatchOperation(ctx context.Context, tool string, args map[string]any, root string) (map[string]any, error) {
	launch := func(id string, generation int64, token string) error {
		return launchHost(root, id, generation, token)
	}
	switch tool {
	case "codex", "codex-reply":
		return runSynchronousContext(ctx, args, tool)
	case "codex_start":
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return nil, homeErr
		}
		override := os.Getenv("PURSER_CODEX_BIN")
		var codexBin any
		if override != "" {
			codexBin = override
		}
		configuredAgent := func() (string, error) {
			config, err := policy.LoadCoderConfig(obsidian.ConfigPath(), filepath.Join(home, ".orchestrator", "config.toml"))
			if err != nil {
				return "", err
			}
			agent, _ := config["coder_agent"].(string)
			return agent, nil
		}
		validateAgent := func(agent string) error {
			if agent == "codex" {
				_, err := jobs.ResolveCodexPrefix(override)
				return err
			}
			_, err := jobs.ResolveAgentPrefix(agent, nil)
			return err
		}
		selectedByEnv := os.Getenv("PURSER_AGENT") != ""
		result, _, err := jobs.Start(root, args, jobs.StartOptions{
			CodexBin: codexBin,
			Launch:   launch,
			Validation: jobs.StartValidationOptions{
				Agent: os.Getenv("PURSER_AGENT"), FixedAgent: selectedByEnv,
				ConfiguredAgent: configuredAgent, ValidateAgent: validateAgent,
			},
			Await: func(id string, record *jobs.Record) map[string]any {
				return jobs.AwaitHost(root, id, record)
			},
		})
		return result, err
	case "codex_result":
		id, ok := args["jobId"].(string)
		if !ok || id == "" {
			return nil, operationInputError{"jobId must be a nonempty string"}
		}
		return jobs.Result(root, id, float64(time.Now().UnixNano())/1e9, jobs.ResultOptions{
			StateRoot: root,
			RecoverJob: func(jobID string) string {
				return jobs.RecoverJob(root, jobID, float64(time.Now().UnixNano())/1e9,
					jobs.RecoverOptions{LaunchHost: func(dir, id string, generation int64, token string) error {
						return launchHostDir(root, dir, id, generation, token)
					}})
			},
		}), nil
	case "codex_cancel":
		return jobs.Cancel(root, args), nil
	case "codex_apply":
		return jobs.Apply(root, args), nil
	default:
		return nil, operationInputError{fmt.Sprintf("unknown operation %q", tool)}
	}
}

func hostMain(argv []string) (code int) {
	jobID := ""
	var generation int64
	if len(argv) > 1 {
		jobID = argv[1]
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := debug.Stack()
			if len(stack) > 4096 {
				stack = stack[:4096]
			}
			err := fmt.Errorf("host panic: %v\n%s", recovered, stack)
			recordHostFailure(jobID, generation, err)
			privateError(err)
			code = 1
		}
	}()
	if len(argv) != 4 {
		return hostFailure(jobID, generation, errors.New("usage: purser-hook host <jobDir> <jobId> <generation> <token>"), 2)
	}
	var err error
	generation, err = strconv.ParseInt(argv[2], 10, 64)
	if err != nil || generation < 1 {
		return hostFailure(jobID, generation, errors.New("generation must be a positive integer"), 2)
	}
	closeContainment, err := containHost()
	if err != nil {
		return hostFailure(jobID, generation, err, 1)
	}
	// The Windows kill-on-close Job Object must stay open until this host exits.
	_ = closeContainment
	status, err := jobs.RunHostGeneration(argv[0], argv[1], generation, argv[3], jobs.HostOptions{HostContained: closeContainment != nil})
	if err != nil {
		return hostFailure(jobID, generation, err, 1)
	}
	writePrivateJSON(status)
	if status["status"] == "error" {
		return 1
	}
	return 0
}

func hostFailure(jobID string, generation int64, err error, code int) int {
	recordHostFailure(jobID, generation, err)
	privateError(err)
	return code
}

func recordHostFailure(jobID string, generation int64, err error) {
	_ = enforce.WriteFailureDiagnostic(stateRoot(), map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339), "error": err.Error(), "phase": "host",
		"job_id": jobID, "generation": generation,
	})
}

func readOperationArgs() (map[string]any, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var envelope map[string]json.RawMessage
	if err = decoder.Decode(&envelope); err != nil || envelope == nil {
		return nil, errors.New("malformed operation JSON: expected an object")
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("malformed operation JSON: trailing data")
	}
	raw, ok := envelope["args"]
	if !ok {
		return nil, errors.New("malformed operation JSON: args object is required")
	}
	var args map[string]any
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&args); err != nil || args == nil {
		return nil, errors.New("malformed operation JSON: args must be an object")
	}
	return args, nil
}

func launchHost(root, id string, generation int64, token string) error {
	return launchHostDir(root, filepath.Join(root, "codex_exec_mcp", "jobs"), id, generation, token)
}

func launchHostDir(root, dir, id string, generation int64, token string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	argv := []string{executable, "host", dir, id, strconv.FormatInt(generation, 10), token}
	_, _, err = jobs.LaunchDetached(argv, os.Environ(), root)
	return err
}

func privateError(err error) {
	writePrivateJSON(map[string]any{"status": "error", "error": err.Error()})
}

func writePrivateJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "{\"status\":\"error\",\"error\":%q}\n", err.Error())
		return
	}
	_, _ = os.Stdout.Write(append(data, '\n'))
}
