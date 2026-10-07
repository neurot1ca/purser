//go:build windows

package proc

import (
	"os/exec"
	"strconv"
	"syscall"
)

func ConfigureNoWindow(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
}

func ConfigureProcessTreeKill(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000200, HideWindow: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000, HideWindow: true}
		if err := kill.Run(); err == nil {
			return nil
		}
		return command.Process.Kill()
	}
}
