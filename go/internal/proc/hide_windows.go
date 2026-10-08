//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func Hide(command *exec.Cmd) *exec.Cmd {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.HideWindow = true
	command.SysProcAttr.CreationFlags |= createNoWindow
	return command
}
