//go:build windows

package proc

import (
	"os/exec"
	"strconv"
	"syscall"
)

func ConfigureNoWindow(command *exec.Cmd) {
	Hide(command)
}

func ConfigureProcessTreeKill(command *exec.Cmd) {
	Hide(command)
	command.SysProcAttr.CreationFlags |= 0x200
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		kill := Hide(exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid)))
		if err := kill.Run(); err == nil {
			return nil
		}
		return command.Process.Kill()
	}
}

func ConfigureBelowNormalPriority(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.CreationFlags |= 0x00004000
}
