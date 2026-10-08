//go:build windows

package testutil

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func Hide(cmd *exec.Cmd) *exec.Cmd {
	attr := &syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		*attr = *cmd.SysProcAttr
	}
	attr.HideWindow = true
	attr.CreationFlags |= createNoWindow
	cmd.SysProcAttr = attr
	return cmd
}
