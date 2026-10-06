//go:build !windows

package review

import "os/exec"

func configureNativeLaunch(_ *exec.Cmd, _ ProcessOptions) {}
