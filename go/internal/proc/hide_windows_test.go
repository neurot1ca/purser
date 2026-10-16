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
	if command.SysProcAttr.CreationFlags != 0x08000200 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want CREATE_NO_WINDOW, preserved flags, and hidden window", command.SysProcAttr)
	}
}

func TestHideClearsIncompatibleConsoleFlags(t *testing.T) {
	command := exec.Command("git")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08004018}

	Hide(command)

	flags := command.SysProcAttr.CreationFlags
	if flags != 0x08004000 || flags&0x10 != 0 || flags&0x8 != 0 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want CREATE_NO_WINDOW, no CREATE_NEW_CONSOLE or DETACHED_PROCESS, and hidden window", command.SysProcAttr)
	}
}
