//go:build windows

package contextio

import (
	"syscall"
)

var maintenanceKernel = syscall.NewLazyDLL("kernel32.dll")
var maintenanceOpenProcess = maintenanceKernel.NewProc("OpenProcess")
var maintenanceWaitProcess = maintenanceKernel.NewProc("WaitForSingleObject")
var maintenanceCloseHandle = maintenanceKernel.NewProc("CloseHandle")

func maintenancePIDAlive(pid int) bool {
	handle, _, openErr := maintenanceOpenProcess.Call(0x00100000|0x00000400, 0, uintptr(uint32(pid)))
	if handle == 0 {
		return openErr == syscall.Errno(5) || openErr != syscall.Errno(87)
	}
	defer maintenanceCloseHandle.Call(handle)
	wait, _, _ := maintenanceWaitProcess.Call(handle, 0)
	return wait != 0
}
