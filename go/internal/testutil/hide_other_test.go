//go:build !windows

package testutil

import (
	"os/exec"
	"testing"
)

func TestHideReturnsCommandUnchanged(t *testing.T) {
	cmd := &exec.Cmd{}
	if got := Hide(cmd); got != cmd || cmd.SysProcAttr != nil {
		t.Fatalf("Hide changed command: %#v", got)
	}
}
