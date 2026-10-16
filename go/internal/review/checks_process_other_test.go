//go:build !windows

package review

import (
	"os"
	"strconv"
	"time"
)

func processExists(pid string) bool {
	n, err := strconv.Atoi(pid)
	if err != nil {
		return true
	}
	proc, err := os.FindProcess(n)
	if err != nil {
		return false
	}
	return proc.Signal(os.Signal(nil)) == nil
}

func waitForProcessExit(pid string, deadline time.Time) bool {
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return processExists(pid)
}
