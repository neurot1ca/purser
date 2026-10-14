//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

func configureWorkerLaunch(command *exec.Cmd, launch Launch) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= launch.CreationFlags
	command.SysProcAttr.HideWindow = command.SysProcAttr.HideWindow || launch.HideWindow
}
