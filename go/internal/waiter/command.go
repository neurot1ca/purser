package waiter

import (
	"os"
	"path/filepath"
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
	return `"` + executable + `" wait ` + jobID
}
