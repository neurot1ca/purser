//go:build windows

package review

import (
	"os/exec"
	"syscall"
)

func configureNativeLaunch(command *exec.Cmd, options ProcessOptions) {
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: options.CreationFlags,
		HideWindow:    options.HideWindow,
	}
}
