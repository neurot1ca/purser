//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

func ConfigureNoWindow(_ *exec.Cmd) {}

func ConfigureProcessTreeKill(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
}

func ConfigureBelowNormalPriority(_ *exec.Cmd) {}
