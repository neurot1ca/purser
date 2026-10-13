//go:build darwin

package proc

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

func ProcessIdentity(pid int) (string, *uint64) {
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
	if length < 37 {
		return ProcessUnconfirmed, nil
	}

	seconds := int64(binary.LittleEndian.Uint64(data[:8]))
	micros := int64(int32(binary.LittleEndian.Uint32(data[8:12])))
	if seconds <= 0 || micros < 0 || micros >= 1_000_000 {
		return ProcessUnconfirmed, nil
	}
	created := uint64(seconds*1_000_000 + micros)
	// KERN_PROC_PID returns a 64-bit extern_proc whose p_stat follows the
	// start-time union, vmspace, sigacts, and p_flag fields. A killed child
	// remains in the process table as SZOMB until its parent waits for it.
	if data[36] == 5 { // SZOMB
		return ProcessDead, &created
	}
	return ProcessAlive, &created
}

func GroupStopped(pid int) (bool, error) {
	if pid <= 0 {
		return false, syscall.EINVAL
	}
	// KERN_PROC_PGRP returns kinfo_proc records. Ignore inert zombies while
	// retaining the unreaped anchor's PID until the last group observation.
	mib := [4]int32{1, 14, 2, int32(pid)}
	var length uintptr
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 4, 0, uintptr(unsafe.Pointer(&length)), 0, 0)
	if errno != 0 {
		return false, errno
	}
	if length == 0 {
		return true, nil
	}
	data := make([]byte, length+648*16)
	length = uintptr(len(data))
	_, _, errno = syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 4, uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&length)), 0, 0)
	if errno != 0 {
		return false, errno
	}
	if length > uintptr(len(data)) || length%648 != 0 {
		return false, syscall.EINVAL
	}
	for offset := uintptr(0); offset < length; offset += 648 {
		if data[offset+36] != 5 {
			return false, nil
		}
	}
	return true, nil
}
