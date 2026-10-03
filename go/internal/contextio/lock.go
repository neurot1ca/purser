package contextio

import (
	"os"
	"path/filepath"
	"time"
)

// LockFile acquires the same one-byte OS lock used by contextio callers.
func LockFile(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if info, statErr := f.Stat(); statErr != nil {
		_ = f.Close()
		return nil, statErr
	} else if info.Size() == 0 {
		if _, err = f.WriteAt([]byte("0"), 0); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err = TryLock(f); err == nil {
			return func() error {
				unlockErr := Unlock(f)
				closeErr := f.Close()
				if unlockErr != nil {
					return unlockErr
				}
				return closeErr
			}, nil
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
