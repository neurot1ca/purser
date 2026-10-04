//go:build windows

package enforce

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	taskUsageKernel32   = syscall.NewLazyDLL("kernel32.dll")
	taskUsageLockProc   = taskUsageKernel32.NewProc("LockFileEx")
	taskUsageUnlockProc = taskUsageKernel32.NewProc("UnlockFileEx")
)

func lockTaskUsageFile(file *os.File) error {
	var overlapped syscall.Overlapped
	r, _, err := taskUsageLockProc.Call(file.Fd(), 0x3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	runtime.KeepAlive(&overlapped)
	if r == 0 {
		return err
	}
	return nil
}

func unlockTaskUsageFile(file *os.File) error {
	var overlapped syscall.Overlapped
	r, _, err := taskUsageUnlockProc.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	runtime.KeepAlive(&overlapped)
	if r == 0 {
		return err
	}
	return nil
}
