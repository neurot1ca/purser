//go:build linux || darwin

package jobs

import (
	"errors"
	"fmt"
	"os/exec"
)

// LaunchDetached starts argv in its own session and returns its process start time.
func LaunchDetached(argv, env []string, dir string) (pid int, identity int64, err error) {
	if len(argv) == 0 || argv[0] == "" {
		return 0, 0, errors.New("detached command is empty")
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = env
	command.Dir = dir
	ConfigureDetachedSession(command)
	if err := command.Start(); err != nil {
		return 0, 0, err
	}
	pid = command.Process.Pid
	state, created := processIdentity(pid)
	if created == nil || state != ProcessAlive {
		_ = command.Process.Kill()
		_ = command.Process.Release()
		return pid, 0, fmt.Errorf("read detached host start time: process identity is %s", state)
	}
	if err := command.Process.Release(); err != nil {
		return pid, int64(*created), err
	}
	return pid, int64(*created), nil
}

// IdentityMatches compares a PID with its platform process start-time identity.
func IdentityMatches(pid int, identity int64) bool {
	if pid <= 0 || identity <= 0 {
		return false
	}
	state, created := processIdentity(pid)
	return state == ProcessAlive && created != nil && int64(*created) == identity
}
