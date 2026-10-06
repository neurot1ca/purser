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
	if len(args) != 1 {
		fmt.Printf("purser-wait %s INVALID_ARGUMENT\n", id)
		return 2
	}
	result := jobs.Wait(stateRoot(), id)
	for _, line := range result.Lines {
		_, _ = fmt.Fprintln(os.Stdout, line)
	}
	return result.ExitCode
}
