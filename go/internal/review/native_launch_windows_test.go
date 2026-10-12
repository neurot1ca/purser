//go:build windows

package review

import "purser-hook/internal/testutil"

import (
	"os/exec"
	"testing"

	"purser-hook/internal/proc"
)

func TestNativeLaunchOptionsPreserveTreeCancel(t *testing.T) {
	for _, tc := range []struct {
		provider string
		flags    uint32
		hide     bool
	}{
		{provider: "agy", flags: 0x10, hide: true},
		{provider: "opencode", flags: 0x08000000, hide: true},
		{provider: "codex", flags: 0x08000000, hide: true},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			adapter := reviewerAdapter{provider: tc.provider}
			command := testutil.Hide(exec.Command("reviewer"))
			proc.ConfigureProcessTreeKill(command)
			configureNativeLaunch(command, adapter.LaunchOptions())
			if command.SysProcAttr == nil || command.SysProcAttr.CreationFlags != tc.flags || !command.SysProcAttr.HideWindow {
				t.Fatalf("launch options = %#v, want flags %#x hidden", command.SysProcAttr, tc.flags)
			}
			if command.Cancel == nil {
				t.Fatal("process-tree cancellation was not preserved")
			}
		})
	}
}
