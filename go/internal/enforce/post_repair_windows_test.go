//go:build windows

package enforce

import (
	"context"
	"testing"
)

func TestPostRepairCommandHidesConsole(t *testing.T) {
	command := newPostRepairCommand(context.Background(), "codex", "exec", "resume")
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags != 0x08000200 {
		t.Fatalf("process attributes = %#v, want hidden window and process-tree flags 0x08000200", command.SysProcAttr)
	}
}
