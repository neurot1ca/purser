//go:build windows

package waiter

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// M3 exports identities only on Unix. Windows identities come from the same
// non-inherited handle used for owner-exit observation.
func OpenOwner(owner Owner) (syscall.Handle, error) {
	handle, err := syscall.OpenProcess(0x100000|0x1000, false, uint32(owner.PID))
	if err != nil {
		return 0, err
	}
	actual, err := handleOwner(handle, owner.PID)
	if err != nil || actual != owner {
		syscall.CloseHandle(handle)
		if err == nil {
			err = fmt.Errorf("session owner identity mismatch")
		}
		return 0, err
	}
	return handle, nil
}
func handleOwner(handle syscall.Handle, pid int) (Owner, error) {
	var created, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &created, &exit, &kernel, &user); err != nil {
		return Owner{}, err
	}
	result, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		return Owner{}, err
	}
	if result != 258 {
		return Owner{}, fmt.Errorf("session owner %d exited", pid)
	}
	return Owner{pid, uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)}, nil
}
func PinOwner(pid int) (Owner, error) {
	if pid <= 0 {
		return Owner{}, syscall.EINVAL
	}
	h, err := syscall.OpenProcess(0x100000|0x1000, false, uint32(pid))
	if err != nil {
		return Owner{}, err
	}
	defer syscall.CloseHandle(h)
	return handleOwner(h, pid)
}
func ownerParent(pid int) (int, string, error) {
	h, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, "", err
	}
	defer syscall.CloseHandle(h)
	entry := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	err = syscall.Process32First(h, &entry)
	for err == nil {
		if int(entry.ProcessID) == pid {
			name := syscall.UTF16ToString(entry.ExeFile[:])
			if strings.EqualFold(name, "node.exe") {
				command, err := ownerCommandLine(pid)
				if err != nil {
					return 0, "", err
				}
				name += " " + command
			}
			return int(entry.ParentProcessID), name, nil
		}
		err = syscall.Process32Next(h, &entry)
	}
	return 0, "", err
}

// ProcessCommandLineInformation avoids WMI and a resident shell when Claude
// uses the npm Node entry point. Treat denied or malformed observation as error.
func ownerCommandLine(pid int) (string, error) {
	h, err := syscall.OpenProcess(0x1000, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(h)
	query := syscall.NewLazyDLL("ntdll.dll").NewProc("NtQueryInformationProcess")
	buffer := make([]byte, 65536+32)
	var required uint32
	status, _, _ := query.Call(uintptr(h), 60, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unsafe.Pointer(&required)))
	if status != 0 {
		return "", fmt.Errorf("session ancestry command line query: NTSTATUS %#x", status)
	}
	type unicodeString struct {
		Length, Maximum uint16
		Buffer          *uint16
	}
	value := (*unicodeString)(unsafe.Pointer(&buffer[0]))
	start := uintptr(unsafe.Pointer(value.Buffer))
	base := uintptr(unsafe.Pointer(&buffer[0]))
	if value.Length%2 != 0 || start < base || start-base > uintptr(len(buffer)) || uintptr(value.Length) > uintptr(len(buffer))-(start-base) {
		return "", fmt.Errorf("malformed session ancestry command line")
	}
	return syscall.UTF16ToString(unsafe.Slice(value.Buffer, int(value.Length)/2)), nil
}
