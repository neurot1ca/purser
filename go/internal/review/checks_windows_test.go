//go:build windows

package review

import (
	"os/exec"
	"syscall"
	"testing"

	"purser-hook/internal/proc"
)

func TestChecksHideWindowsConsole(t *testing.T) {
	status := exec.Command("git")
	proc.ConfigureNoWindow(status)
	if status.SysProcAttr == nil || status.SysProcAttr.CreationFlags&0x08000000 == 0 || !status.SysProcAttr.HideWindow {
		t.Fatalf("git status process attributes = %#v, want CREATE_NO_WINDOW and hidden window", status.SysProcAttr)
	}

	check := exec.Command("check")
	proc.ConfigureProcessTreeKill(check)
	if check.SysProcAttr == nil || check.SysProcAttr.CreationFlags&0x08000000 == 0 || check.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 || !check.SysProcAttr.HideWindow {
		t.Fatalf("check process attributes = %#v, want CREATE_NO_WINDOW, CREATE_NEW_PROCESS_GROUP, and hidden window", check.SysProcAttr)
	}
}
