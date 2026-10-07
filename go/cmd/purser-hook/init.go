package main

import (
	"os"

	"purser-hook/internal/initsetup"
)

func initMain(args []string) int { return initsetup.Run(os.Stdout, os.Stderr, args) }
