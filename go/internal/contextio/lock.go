package contextio

import (
	"os"
	"path/filepath"
	"time"
)

// LockFile acquires the same one-byte OS lock used by contextio callers.
func LockFile(path string) (func() error, error) {
	return lockFile(path, false)
}

// LockFileBlocking waits for another owner to release the file lock.
func LockFileBlocking(path string) (func() error, error) {
	return lockFile(path, true)
}

func lockFile(path string, blocking bool) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if blocking {
		if err = lockBlocking(f); err == nil {
			return unlockFile(f), nil
		}
		_ = f.Close()
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err = TryLock(f); err == nil {
			return unlockFile(f), nil
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func unlockFile(f *os.File) func() error {
	return func() error {
		unlockErr := Unlock(f)
		closeErr := f.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}
}
