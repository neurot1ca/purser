//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

const createNewConsole = 0x10

func Hide(command *exec.Cmd) *exec.Cmd {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.HideWindow = true
	command.SysProcAttr.CreationFlags = command.SysProcAttr.CreationFlags&^(0x08000000|0x8) | createNewConsole
	return command
}
