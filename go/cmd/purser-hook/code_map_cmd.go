package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
)

func codeMapMain(args []string) int {
	flags := flag.NewFlagSet("code-map", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repo := flags.String("repo", "", "repository path")
	budgetSeconds := flags.Float64("budget-seconds", 60, "completion budget in seconds")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *repo == "" || *budgetSeconds <= 0 || math.IsNaN(*budgetSeconds) || math.IsInf(*budgetSeconds, 0) {
		return codeMapError(fmt.Errorf("usage: purser-hook code-map --repo <path> [--budget-seconds N]"))
	}
	if *budgetSeconds > 60 {
		*budgetSeconds = 60
	}
	root, err := filepath.Abs(*repo)
	if err != nil {
		return codeMapError(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return codeMapError(err)
	}
	marker := filepath.Join(stateRoot(), "projects", contextio.ProjectKey(root), "code-map", "completion.lock")
	defer os.Remove(marker)
	path, status := enforce.RunCodeMapCompletion(root, stateRoot(), time.Duration(*budgetSeconds*float64(time.Second)))
	if _, err = fmt.Fprintf(os.Stdout, "CODE_MAP=%s STATUS=%s\n", path, status); err != nil {
		return 1
	}
	if status == "UNAVAILABLE" {
		return 1
	}
	return 0
}

func codeMapError(err error) int {
	_, _ = fmt.Fprintln(os.Stderr, "Code map completion failed: "+err.Error())
	return 2
}
