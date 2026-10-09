package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"purser-hook/internal/pilotreport"
)

func pilotReportMain(args []string) int {
	var jobs []string
	for i := 0; i < len(args); i++ {
		if args[i] != "--job" || i+1 >= len(args) || args[i+1] == "" {
			_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook pilot-report --job JOB_ID [--job JOB_ID ...]")
			return 2
		}
		jobs = append(jobs, args[i+1])
		i++
	}
	if len(jobs) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook pilot-report --job JOB_ID [--job JOB_ID ...]")
		return 2
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		config = filepath.Join(home, ".claude")
	}
	encoder := json.NewEncoder(os.Stdout)
	for _, job := range jobs {
		if err := encoder.Encode(pilotreport.Build(stateRoot(), filepath.Join(config, "projects"), job)); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}
