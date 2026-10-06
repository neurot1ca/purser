//go:build windows

package proc

import (
	"os/exec"
	"syscall"
)

func configureDetached(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000008, HideWindow: true}
}
