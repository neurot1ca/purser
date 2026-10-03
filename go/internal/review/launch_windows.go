//go:build windows

package review

import (
	"os/exec"
	"syscall"
)

func configureLaunch(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x10, HideWindow: true}
}
