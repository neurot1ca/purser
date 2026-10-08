package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/maintenance"
)

type maintenanceJobs struct{}

func (maintenanceJobs) Start(_ string, args map[string]any) (map[string]any, error) {
	return dispatchOperation(context.Background(), "codex_start", args, stateRoot())
}
func (maintenanceJobs) Result(_, id string) map[string]any {
	value, _ := dispatchOperation(context.Background(), "codex_result", map[string]any{"jobId": id}, stateRoot())
	return value
}
func (maintenanceJobs) Cancel(_, id string) map[string]any {
	value, _ := dispatchOperation(context.Background(), "codex_cancel", map[string]any{"jobId": id}, stateRoot())
	return value
}

func maintenanceMain(args []string) int {
	if len(args) == 0 {
		return maintenanceUsage("")
	}
	command := args[0]
	args = args[1:]
	repo, report, decision := "", "", ""
	seenReport := false
	var acted []string
	reportType := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo", "--report", "--acted":
			if i+1 >= len(args) {
				return maintenanceUsage("argument " + args[i] + ": expected one argument")
			}
			i++
			switch args[i-1] {
			case "--repo":
				repo = args[i]
			case "--report":
				report = args[i]
				seenReport = true
			case "--acted":
				acted = append(acted, args[i])
			}
		default:
			if command == "consent" && decision == "" {
				decision = args[i]
			} else if command == "run" && reportType == "" {
				reportType = args[i]
			} else {
				return maintenanceUsage("unrecognized arguments: " + args[i])
			}
		}
	}
	if repo == "" {
		return maintenanceUsage("the following arguments are required: --repo")
	}
	if command == "skip-second-show" || command == "mark-done" {
		if !seenReport {
			return maintenanceUsage("the following arguments are required: --report")
		}
	} else if seenReport {
		return maintenanceUsage("unrecognized arguments: --report")
	}
	if command != "mark-done" && len(acted) > 0 {
		return maintenanceUsage("unrecognized arguments: --acted")
	}
	if command == "consent" && decision == "" {
		return maintenanceUsage("the following arguments are required: decision")
	}
	if command == "run" && reportType == "" {
		return maintenanceUsage("the following arguments are required: report_type")
	}
	if command == "consent" && decision != "yes" && decision != "no" {
		return maintenanceUsage("argument decision: invalid choice: " + strconv.Quote(decision) + " (choose from 'yes', 'no')")
	}
	if command == "run" && reportType != "cleanup" && reportType != "test-gaps" {
		return maintenanceUsage("argument report_type: invalid choice: " + strconv.Quote(reportType) + " (choose from 'cleanup', 'test-gaps')")
	}
	state := stateRoot()
	if command == "run" {
		root, err := maintenance.CanonicalRepository(repo)
		if err == nil {
			err = maintenance.RequireOptIn(state, root)
		}
		if err == nil {
			var path string
			path, err = maintenance.RunAndPersistReport(maintenance.Runner{Jobs: maintenanceJobs{}}, state, root, reportType)
			if err == nil && path != "" {
				fmt.Fprintln(os.Stdout, path)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "maintenance report failed: "+err.Error())
			return 1
		}
		return 0
	}
	if command == "check" {
		root, err := maintenance.CanonicalRepository(repo)
		if err == nil {
			_, err = maintenance.DelayedCheck(root, state, 5*time.Minute, time.Now(), maintenance.LatestQuota(time.Now()), maintenance.Runner{Jobs: maintenanceJobs{}})
		}
		if errors.Is(err, os.ErrExist) {
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "maintenance report failed: "+err.Error())
			return 1
		}
		return 0
	}
	if command != "opt-in" && command != "opt-out" && command != "consent" && command != "skip-second-show" && command != "mark-done" {
		return maintenanceUsage("invalid choice: " + strconv.Quote(command) + " (choose from 'opt-in', 'opt-out', 'consent', 'skip-second-show', 'mark-done', 'run', 'check')")
	}
	path, err := maintenance.Control(state, command, repo, decision, report, acted)
	if err != nil {
		fmt.Fprintln(os.Stderr, "maintenance report failed: "+err.Error())
		return 1
	}
	if path != "" {
		fmt.Fprintln(os.Stdout, path)
	}
	return 0
}

func maintenanceUsage(message string) int {
	if message != "" {
		fmt.Fprintf(os.Stderr, "usage: maintenance %s\nmaintenance: error: %s\n", strings.Join(os.Args[2:], " "), message)
	} else {
		fmt.Fprintln(os.Stderr, "usage: maintenance {opt-in,opt-out,consent,skip-second-show,mark-done,run,check} ...")
	}
	return 2
}
