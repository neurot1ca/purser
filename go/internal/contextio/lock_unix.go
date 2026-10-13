//go:build !windows

package contextio

import (
	"encoding/json"
	"os"
	"strconv"
	"syscall"
)

func tryLock(f *os.File) error      { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
func lockBlocking(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
func unlock(f *os.File) error       { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func TryLock(f *os.File) error      { return tryLock(f) }
func Unlock(f *os.File) error       { return unlock(f) }
func fileIdentity(_ string, info os.FileInfo) any {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino <= 0 {
		return nil
	}
	return []any{json.Number(strconv.FormatUint(uint64(stat.Dev), 10)), json.Number(strconv.FormatUint(stat.Ino, 10))}
}
