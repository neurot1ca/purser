package main

import (
	"os"

	"purser-hook/internal/initsetup"
)

func reviewerMain(args []string) int {
	return initsetup.ReviewerRun(os.Stdout, os.Stderr, args)
}
