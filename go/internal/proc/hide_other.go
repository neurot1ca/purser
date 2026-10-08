//go:build !windows

package proc

import "os/exec"

func Hide(command *exec.Cmd) *exec.Cmd { return command }
