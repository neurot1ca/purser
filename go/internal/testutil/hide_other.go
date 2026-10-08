//go:build !windows

package testutil

import "os/exec"

func Hide(cmd *exec.Cmd) *exec.Cmd { return cmd }
