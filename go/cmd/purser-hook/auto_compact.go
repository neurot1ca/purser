package main

import (
	"os"

	"purser-hook/internal/initsetup"
)

func autoCompactMain(args []string) int {
	return initsetup.AutoCompactRun(args, os.Stdout, os.Stderr)
}
