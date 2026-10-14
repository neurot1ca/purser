//go:build windows

package review

import (
	"os/exec"
	"syscall"
)

func configureNativeLaunch(command *exec.Cmd, options ProcessOptions) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= options.CreationFlags
	command.SysProcAttr.HideWindow = command.SysProcAttr.HideWindow || options.HideWindow
}
