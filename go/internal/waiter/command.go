package waiter

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Command returns the runnable command for the waiter of jobID.
func Command(jobID string) string {
	executable := "purser-hook"
	if path, err := os.Executable(); err == nil {
		if path, err = filepath.Abs(path); err == nil {
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				executable = resolved
			}
		}
	}
	token := "unknown"
	if owner, err := SessionOwner(); err == nil {
		token = owner.String()
	}
	return `"` + executable + `" wait ` + jobID + ` --session-owner ` + token
}

// ValidationCommand returns a shell-ready command for the bounded apply-validation waiter.
func ValidationCommand(stateRoot, repositoryKey, jobID string) string {
	executable := "purser-hook"
	if path, err := os.Executable(); err == nil {
		if path, err = filepath.Abs(path); err == nil {
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				executable = resolved
			}
		}
	}
	if runtime.GOOS == "windows" {
		return "& " + powerShellQuote(executable) + " wait-validation " + powerShellQuote(stateRoot) + " " + powerShellQuote(repositoryKey) + " " + powerShellQuote(jobID)
	}
	return shellQuote(executable) + " wait-validation " + shellQuote(stateRoot) + " " + shellQuote(repositoryKey) + " " + shellQuote(jobID)
}

func powerShellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
