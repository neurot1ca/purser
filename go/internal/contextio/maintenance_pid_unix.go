//go:build !windows

package contextio

import (
	"errors"
	"syscall"
)

func maintenancePIDAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM) || !errors.Is(err, syscall.ESRCH)
}
