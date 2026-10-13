package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"purser-hook/internal/contextio"
)

func coderEffortMain(args []string) int {
	usage := "usage: purser-hook coder-effort low|medium|high|xhigh|auto --cwd <repository>"
	effort := ""
	if len(args) > 0 && args[0][0] != '-' {
		if !validCoderEffortArgument(args[0]) {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		effort = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("coder-effort", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "repository path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *cwd == "" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	repository, err := gearRepository(*cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	config, err := gearConfig(repository)
	if err != nil {
		writeJSON(map[string]any{"error": err.Error()})
		return 1
	}
	if config["coder_agent"] != "codex" {
		writeJSON(map[string]any{"error": "coder_effort applies only to the Codex coder"})
		return 1
	}
	if effort == "" {
		effort, err = contextio.CoderEffort(stateRoot(), contextio.ProjectKey(repository))
		if err != nil {
			writeJSON(map[string]any{"error": err.Error()})
			return 1
		}
		if effort == "" {
			effort = "auto"
		}
	} else if err := contextio.SetCoderEffort(stateRoot(), contextio.ProjectKey(repository), effort); err != nil {
		writeJSON(map[string]any{"error": err.Error()})
		return 1
	}
	ack := map[string]any{"coder_effort": effort}
	if effort != "auto" {
		ack["applies_to"] = "new Codex jobs"
	}
	data, err := json.Marshal(ack)
	if err != nil {
		return 1
	}
	_, err = fmt.Fprintln(os.Stdout, string(data))
	if err != nil {
		return 1
	}
	return 0
}

func validCoderEffortArgument(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh", "auto":
		return true
	default:
		return false
	}
}
