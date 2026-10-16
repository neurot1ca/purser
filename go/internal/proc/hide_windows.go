//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func Hide(command *exec.Cmd) *exec.Cmd {
	// agy's background updater can open its own console despite CREATE_NO_WINDOW.
	// Inherit the documented opt-out through guards and wrappers as well.
	command.Env = append(command.Environ(), "AGY_CLI_DISABLE_AUTO_UPDATE=true")
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.HideWindow = true
	command.SysProcAttr.CreationFlags = command.SysProcAttr.CreationFlags&^(0x8|0x10) | createNoWindow
	return command
}
