package main

import (
	"os"
	"path/filepath"

	"purser-hook/internal/optimize"
)

func optimizeMain(args []string) int {
	root, err := os.Getwd()
	if err != nil {
		_, _ = os.Stderr.WriteString("optimize: cannot resolve project directory\n")
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = os.Stderr.WriteString("optimize: cannot resolve home directory\n")
		return 1
	}
	projects := filepath.Join(home, ".claude", "projects")
	if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" {
		projects = filepath.Join(config, "projects")
	}
	if len(args) > 0 {
		_, _ = os.Stderr.WriteString("optimize: this command takes no arguments\n")
		return 2
	}
	if err := optimize.Run(os.Stdout, projects, home, root, 30); err != nil {
		_, _ = os.Stderr.WriteString("optimize: report failed\n")
		return 1
	}
	return 0
}
