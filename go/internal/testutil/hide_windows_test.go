//go:build windows

package testutil

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestHideMergesWindowsProcessAttributes(t *testing.T) {
	cmd := &exec.Cmd{SysProcAttr: &syscall.SysProcAttr{CreationFlags: 0x200}}
	if got := Hide(cmd); got != cmd {
		t.Fatal("Hide returned a different command")
	}
	if !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags != 0x08000200 {
		t.Fatalf("SysProcAttr = %#v", cmd.SysProcAttr)
	}
}
