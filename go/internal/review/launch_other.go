//go:build !windows

package review

import "os/exec"

func configureLaunch(command *exec.Cmd) {}
