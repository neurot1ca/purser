package main

import (
	"os"
	"path/filepath"

	"purser-hook/internal/checkreport"
)

func checkMain(args []string) int {
	jsonOutput := false
	if len(args) == 1 && args[0] == "--json" {
		jsonOutput = true
	} else if len(args) != 0 {
		_, _ = os.Stderr.WriteString("check: usage: purser-hook check [--json]\n")
		return 2
	}
	root, err := os.Getwd()
	if err != nil {
		_, _ = os.Stderr.WriteString("check: cannot resolve project directory\n")
		return 1
	}
	pluginRoot := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if pluginRoot == "" {
		executable, err := os.Executable()
		if err != nil {
			_, _ = os.Stderr.WriteString("check: cannot resolve plugin directory\n")
			return 1
		}
		pluginRoot = filepath.Dir(filepath.Dir(executable))
	}
	report := checkreport.Run(root, pluginRoot)
	if jsonOutput {
		if err := checkreport.WriteJSON(os.Stdout, report); err != nil {
			return 1
		}
	} else {
		if _, err := os.Stdout.WriteString(checkreport.RenderMarkdown(report) + "\n"); err != nil {
			return 1
		}
	}
	return report.ExitCode
}
