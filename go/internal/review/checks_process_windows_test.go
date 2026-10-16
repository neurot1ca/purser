//go:build windows

package review

import (
	"strconv"
	"syscall"
	"time"
)

const (
	processQueryLimitedInformation = 0x1000
	windowsErrorInvalidParameter   = syscall.Errno(87)
)

func openProcessForWait(pid string) (syscall.Handle, bool) {
	n, err := strconv.ParseUint(pid, 10, 32)
	if err != nil {
		return 0, true
	}
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE|processQueryLimitedInformation, false, uint32(n))
	if err == windowsErrorInvalidParameter {
		return 0, false
	}
	if err != nil {
		return 0, true
	}
	return h, true
}

func processExists(pid string) bool {
	h, exists := openProcessForWait(pid)
	if !exists {
		return false
	}
	if h == 0 {
		return true
	}
	defer syscall.CloseHandle(h)
	event, err := syscall.WaitForSingleObject(h, 0)
	return err != nil || event != syscall.WAIT_OBJECT_0
}

func waitForProcessExit(pid string, deadline time.Time) bool {
	h, exists := openProcessForWait(pid)
	if !exists {
		return false
	}
	if h == 0 {
		return true
	}
	defer syscall.CloseHandle(h)
	remaining := time.Until(deadline)
	if remaining <= 0 {
		event, err := syscall.WaitForSingleObject(h, 0)
		return err != nil || event != syscall.WAIT_OBJECT_0
	}
	milliseconds := uint64((remaining + time.Millisecond - 1) / time.Millisecond)
	if milliseconds > uint64(^uint32(0)) {
		milliseconds = uint64(^uint32(0))
	}
	event, err := syscall.WaitForSingleObject(h, uint32(milliseconds))
	return err != nil || event != syscall.WAIT_OBJECT_0
}
