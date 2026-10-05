package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"purser-hook/internal/jobs"
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
	root := stateRoot()
	launch := func(id string, generation int64, token string) error {
		return launchHost(root, id, generation, token)
	}
	var result map[string]any
	switch argv[0] {
	case "codex_start":
		result, _, err = jobs.Start(root, args, jobs.StartOptions{Launch: launch})
	case "codex_result":
		id, ok := args["jobId"].(string)
		if !ok || id == "" {
			privateError(errors.New("jobId must be a nonempty string"))
			return 2
		}
		result = jobs.Result(root, id, float64(time.Now().UnixNano())/1e9, jobs.ResultOptions{
			StateRoot: root,
			RecoverJob: func(jobID string) string {
				return jobs.RecoverJob(root, jobID, float64(time.Now().UnixNano())/1e9,
					jobs.RecoverOptions{LaunchHost: func(dir, id string, generation int64, token string) error {
						return launchHostDir(root, dir, id, generation, token)
					}})
			},
		})
	case "codex_cancel":
		result = jobs.Cancel(root, args)
	case "codex_apply":
		result = jobs.Apply(root, args)
	default:
		privateError(fmt.Errorf("unknown operation %q", argv[0]))
		return 2
	}
	if err != nil {
		privateError(err)
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

func hostMain(argv []string) int {
	if len(argv) != 4 {
		privateError(errors.New("usage: purser-hook host <jobDir> <jobId> <generation> <token>"))
		return 2
	}
	generation, err := strconv.ParseInt(argv[2], 10, 64)
	if err != nil || generation < 1 {
		privateError(errors.New("generation must be a positive integer"))
		return 2
	}
	closeContainment, err := containHost()
	if err != nil {
		privateError(err)
		return 1
	}
	// The Windows kill-on-close Job Object must stay open until this host exits.
	_ = closeContainment
	status, err := jobs.RunHostGeneration(argv[0], argv[1], generation, argv[3], jobs.HostOptions{HostContained: closeContainment != nil})
	if err != nil {
		privateError(err)
		return 1
	}
	writePrivateJSON(status)
	if status["status"] == "error" {
		return 1
	}
	return 0
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
