//go:build windows

package contextio

import (
	"encoding/json"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	lockFileEx    = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	unlockFileEx  = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
	getFileInfoEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetFileInformationByHandleEx")
)

func tryLock(f *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, e := lockFileEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r1 == 0 {
		return e
	}
	return nil
}
func unlock(f *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, e := unlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r1 == 0 {
		return e
	}
	return nil
}

func TryLock(f *os.File) error { return tryLock(f) }
func Unlock(f *os.File) error  { return unlock(f) }

type fileIDInfo struct {
	Volume uint64
	ID     [16]byte
}

func fileIdentity(path string, _ os.FileInfo) any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var id fileIDInfo
	r, _, _ := getFileInfoEx.Call(f.Fd(), 18, uintptr(unsafe.Pointer(&id)), unsafe.Sizeof(id))
	if r == 0 {
		return nil
	}
	return []any{json.Number(strconv.FormatUint(id.Volume, 10)), json.Number(strconv.FormatUint(*(*uint64)(unsafe.Pointer(&id.ID[0])), 10))}
}
