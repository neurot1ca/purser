package main

import (
	"fmt"
	"os"

	"purser-hook/internal/enforce"
)

func mechanicalShadowMain(args []string) int {
	if len(args) != 1 || args[0] == "" {
		_, _ = fmt.Fprintln(os.Stderr, "usage: purser-hook mechanical-shadow JOB.json")
		return 2
	}
	if enforce.RunMechanicalShadowJob(args[0]) {
		return 0
	}
	return 1
}
