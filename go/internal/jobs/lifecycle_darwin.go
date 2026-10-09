//go:build darwin

package jobs

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

func processIdentity(pid int) (string, *uint64) {
	if pid <= 0 {
		return ProcessUnconfirmed, nil
	}

	mib := [4]int32{1, 14, 1, int32(pid)}
	data := make([]byte, 648)
	length := uintptr(len(data))
	_, _, errno := syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(len(mib)),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(unsafe.Pointer(&length)),
		0,
		0,
	)
	if errno != 0 {
		if errno == syscall.ESRCH {
			return ProcessDead, nil
		}
		return ProcessUnconfirmed, nil
	}
	if length == 0 {
		return ProcessDead, nil
	}
	if length < 12 {
		return ProcessUnconfirmed, nil
	}

	seconds := int64(binary.LittleEndian.Uint64(data[:8]))
	micros := int64(int32(binary.LittleEndian.Uint32(data[8:12])))
	if seconds <= 0 || micros < 0 || micros >= 1_000_000 {
		return ProcessUnconfirmed, nil
	}
	created := uint64(seconds*1_000_000 + micros)
	return ProcessAlive, &created
}

func ProcessGroupStopped(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == syscall.ESRCH
}
