//go:build !windows

package proc

import "os/exec"

func configureWorkerLaunch(_ *exec.Cmd, _ Launch) {}
