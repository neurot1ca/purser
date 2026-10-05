//go:build !windows

package jobs

import (
	"os"
	"path/filepath"
	"syscall"
)

type OwnershipLock struct{ file *os.File }

func AcquireOwnership(dir, jobID string) (*OwnershipLock, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, jobID+".owner-lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &OwnershipLock{file: f}, nil
}

func (l *OwnershipLock) Release() error {
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
