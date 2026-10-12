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
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x10}

	if got := Hide(command); got != command {
		t.Fatal("Hide did not return the command")
	}
	if command.SysProcAttr.CreationFlags != 0x08000010 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want flags 0x08000010 and hidden window", command.SysProcAttr)
	}
}
