//go:build windows

package proc

import "purser-hook/internal/testutil"

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestHideMergesProcessAttributes(t *testing.T) {
	command := testutil.Hide(exec.Command("git"))
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x200}

	if got := Hide(command); got != command {
		t.Fatal("Hide did not return the command")
	}
	if command.SysProcAttr.CreationFlags != 0x210 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want flags 0x210 and hidden window", command.SysProcAttr)
	}
}

func TestHideClearsIncompatibleConsoleFlags(t *testing.T) {
	command := exec.Command("git")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08004008}

	Hide(command)

	if command.SysProcAttr.CreationFlags != 0x4010 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want flags 0x4010 and hidden window", command.SysProcAttr)
	}
}
