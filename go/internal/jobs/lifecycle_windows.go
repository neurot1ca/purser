//go:build windows

package jobs

import (
	"errors"
	"syscall"
)

func recoveryProcessState(pid int, createdAt uint64) string {
	actual, alive, err := processIdentity(uint32(pid))
	if err != nil {
		if errors.Is(err, syscall.Errno(87)) {
			return "DEAD"
		}
		return "UNCONFIRMED"
	}
	if !alive || actual != int64(createdAt) {
		return "DEAD"
	}
	return "ALIVE"
}

func recoveryGroupStopped(int) bool { return false }
