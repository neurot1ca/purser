package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"purser-hook/internal/contextio"
)

func quotaWakeMain(args []string) int {
	flags := flag.NewFlagSet("quota-wake", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "repository path")
	clear := flags.Bool("clear", false, "remove the pending wake request")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *cwd == "" {
		_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook quota-wake --cwd <repository> [--clear]")
		return 2
	}
	repository, err := gearRepository(*cwd)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *clear {
		err = contextio.ClearQuotaWake(repository, stateRoot())
	} else {
		var wake *contextio.QuotaWake
		wake, err = contextio.ReadQuotaWake(repository, stateRoot(), time.Now().UTC())
		if err == nil {
			if wake == nil {
				writePrivateJSON(map[string]any{"pending": false})
			} else {
				writePrivateJSON(map[string]any{"pending": true, "reset_at": wake.ResetAt, "task_id": wake.TaskID, "reason": wake.Reason})
			}
			return 0
		}
	}
	if err != nil {
		privateError(err)
		return 1
	}
	writePrivateJSON(map[string]any{"pending": false})
	return 0
}
