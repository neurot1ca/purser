package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"purser-hook/internal/contextio"
)

func planMain(args []string) int {
	if len(args) == 0 {
		return planError(fmt.Errorf("usage: purser-hook plan accept|stop|resume|complete-milestone|gate|status --cwd <repository>"))
	}
	action := args[0]
	if action != "accept" && action != "stop" && action != "resume" && action != "complete-milestone" && action != "gate" && action != "status" {
		return planError(fmt.Errorf("unsupported plan action %q", action))
	}
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "repository path")
	taskID := flags.String("task-id", "", "plan task id")
	artifact := flags.String("artifact", "", "accepted plan artifact path")
	milestones := flags.String("milestones", "", "comma-separated accepted milestone numbers")
	revision := flags.Int("revision", 0, "expected active-plan revision")
	value := flags.String("value", "", "gate detail")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *cwd == "" {
		return planError(fmt.Errorf("invalid plan arguments"))
	}
	repository, err := gearRepository(*cwd)
	if err != nil {
		return planError(err)
	}
	var state *contextio.PlanState
	if action == "accept" {
		parsed, err := parseMilestones(*milestones)
		if err != nil {
			return planError(err)
		}
		state, err = contextio.AcceptPlan(stateRoot(), repository, *taskID, *artifact, parsed)
		if err != nil {
			return planError(err)
		}
	} else if action == "status" {
		state, err = contextio.ReadPlan(stateRoot(), repository)
		if err != nil {
			return planError(err)
		}
	} else {
		state, err = contextio.UpdatePlan(stateRoot(), repository, *taskID, *revision, action, *value)
		if err != nil {
			return planError(err)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(state); err != nil {
		return planError(err)
	}
	return 0
}

func parseMilestones(raw string) ([]int, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("--milestones is required")
	}
	parts := strings.Split(raw, ",")
	values := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || value < 1 {
			return nil, fmt.Errorf("invalid accepted milestone %q", part)
		}
		values = append(values, value)
	}
	return values, nil
}

func planError(err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "Plan state could not be updated: %v\n", err)
	return 1
}
