//go:build windows

package jobs

import "purser-hook/internal/proc"

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	createBreakawayFromJob = 0x01000000
	createNewProcessGroup  = 0x00000200
	processCreateProcess   = 0x00000080
	processDupHandle       = 0x00000040
	processQueryLimited    = 0x00001000
	processSynchronize     = 0x00100000
	waitObject0            = 0
	waitTimeout            = 0x00000102
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	user32              = syscall.NewLazyDLL("user32.dll")
	getShellWindow      = user32.NewProc("GetShellWindow")
	getWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	waitForSingleObject = kernel32.NewProc("WaitForSingleObject")
)

// LaunchDetached starts argv as a host detached from the caller's process tree.
// identity is the Windows creation FILETIME, matching hostCreatedAt in Python records.
func LaunchDetached(argv, env []string, dir string) (pid int, identity int64, err error) {
	if len(argv) == 0 || argv[0] == "" {
		return 0, 0, errors.New("detached command is empty")
	}
	parent, err := detachedParent()
	if err != nil {
		return 0, 0, err
	}
	defer syscall.CloseHandle(parent)

	command := proc.Hide(exec.Command(argv[0], argv[1:]...))
	command.Env = env
	command.Dir = dir
	command.SysProcAttr.CreationFlags |= createBreakawayFromJob | createNewProcessGroup
	// ParentProcess makes Go use STARTUPINFOEX and set EXTENDED_STARTUPINFO_PRESENT.
	command.SysProcAttr.ParentProcess = parent
	if len(argv) > 1 && argv[1] == "validate-apply" {
		// The validation host's below-normal priority is inherited by its guards and workers.
		proc.ConfigureBelowNormalPriority(command)
	}
	proc.Hide(command)
	if err := command.Start(); err != nil {
		return 0, 0, err
	}
	pid = command.Process.Pid
	identity, alive, err := processIdentity(uint32(pid))
	if err != nil || !alive {
		_ = command.Process.Kill()
		_ = command.Process.Release()
		if err == nil {
			err = errors.New("detached host exited before identity was recorded")
		}
		return pid, 0, fmt.Errorf("read detached host creation time: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return pid, identity, err
	}
	return pid, identity, nil
}

// IdentityMatches rejects dead, inaccessible, and PID-reused processes.
func IdentityMatches(pid int, identity int64) bool {
	if pid <= 0 || identity <= 0 {
		return false
	}
	actual, alive, err := processIdentity(uint32(pid))
	return err == nil && alive && actual == identity
}

func detachedParent() (syscall.Handle, error) {
	window, _, _ := getShellWindow.Call()
	var pid uint32
	if window != 0 {
		getWindowThreadPID.Call(window, uintptr(unsafe.Pointer(&pid)))
	}
	if pid == 0 {
		if fallback := os.Getenv("PURSER_CODEX_TEST_PARENT_PID"); fallback != "" {
			parsed, err := strconv.ParseUint(fallback, 10, 32)
			if err != nil || parsed == 0 {
				return 0, errors.New("invalid detached parent process id")
			}
			pid = uint32(parsed)
		}
	}
	if pid == 0 {
		return 0, errors.New("desktop shell parent unavailable")
	}
	// Go duplicates the child's std handles into ParentProcess, which needs PROCESS_DUP_HANDLE.
	parent, err := syscall.OpenProcess(processCreateProcess|processDupHandle, false, pid)
	if err != nil {
		return 0, fmt.Errorf("open detached parent process: %w", err)
	}
	return parent, nil
}

func processIdentity(pid uint32) (int64, bool, error) {
	handle, err := syscall.OpenProcess(processQueryLimited|processSynchronize, false, pid)
	if err != nil {
		return 0, false, err
	}
	defer syscall.CloseHandle(handle)
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, false, err
	}
	wait, _, _ := waitForSingleObject.Call(uintptr(handle), 0)
	if wait != waitTimeout && wait != waitObject0 {
		return 0, false, fmt.Errorf("wait for process identity: unexpected result %#x", wait)
	}
	return int64(created.HighDateTime)<<32 | int64(created.LowDateTime), wait == waitTimeout, nil
}
