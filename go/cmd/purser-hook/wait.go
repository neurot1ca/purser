package main

import (
	"fmt"
	"os"

	"purser-hook/internal/jobs"
)

func waitMain(args []string) int {
	id := "invalid"
	if len(args) > 0 {
		id = args[0]
	}
	if len(args) != 1 && (len(args) != 3 || args[1] != "--session-owner") {
		fmt.Printf("purser-wait %s INVALID_ARGUMENT\n", id)
		return 2
	}
	if len(args) == 3 {
		_ = os.Setenv("PURSER_SESSION_OWNER", args[2])
	}
	ctx, cancel, err := sessionContext(os.Stderr)
	if err != nil {
		return 2
	}
	defer cancel()
	result := jobs.WaitContext(ctx, stateRoot(), id)
	for _, line := range result.Lines {
		_, _ = fmt.Fprintln(os.Stdout, line)
	}
	return result.ExitCode
}
