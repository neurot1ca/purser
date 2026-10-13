//go:build windows

package proc

import "os/exec"

func configureDetached(command *exec.Cmd) {
	Hide(command)
}
