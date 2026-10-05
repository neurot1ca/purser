//go:build windows

package jobs

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

var lockKernel = syscall.NewLazyDLL("kernel32.dll")
var lockFileEx = lockKernel.NewProc("LockFileEx")
var unlockFileEx = lockKernel.NewProc("UnlockFileEx")

type OwnershipLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func AcquireOwnership(dir, jobID string) (*OwnershipLock, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dir+string(os.PathSeparator)+jobID+".owner-lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	l := &OwnershipLock{file: f}
	for {
		r, _, callErr := lockFileEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&l.overlapped)))
		if r != 0 {
			return l, nil
		}
		if callErr != syscall.Errno(33) {
			f.Close()
			return nil, callErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *OwnershipLock) Release() error {
	r, _, err := unlockFileEx.Call(l.file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&l.overlapped)))
	closeErr := l.file.Close()
	if r == 0 {
		return err
	}
	return closeErr
}
