//go:build !windows

package jobs

import (
	"errors"
	"os/exec"
	"syscall"
)

const (
	ProcessAlive       = "ALIVE"
	ProcessDead        = "DEAD"
	ProcessUnconfirmed = "UNCONFIRMED"
)

// ConfigureDetachedSession starts the command in its own session and process
// group, matching the bridge's start_new_session=True host launch.
func ConfigureDetachedSession(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// KillProcessGroup forcefully stops the detached session's process group.
func KillProcessGroup(pid int) error {
	if pid <= 0 {
		return errors.New("process group PID must be positive")
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// ClassifyProcessIdentity returns DEAD when the PID is absent or has been
// reused, and otherwise returns the platform's identity state.
func ClassifyProcessIdentity(pid int, expectedStartTime uint64) string {
	state, actualStartTime := processIdentity(pid)
	if actualStartTime != nil && *actualStartTime != expectedStartTime {
		return ProcessDead
	}
	return state
}

func recoveryProcessState(pid int, createdAt uint64) string {
	return ClassifyProcessIdentity(pid, createdAt)
}

func recoveryGroupStopped(pid int) bool { return ProcessGroupStopped(pid) }
