//go:build windows

package enforce

import (
	"context"
	"testing"
)

func TestPostRepairCommandHidesConsole(t *testing.T) {
	command := newPostRepairCommand(context.Background(), "codex", "exec", "resume")
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags != 0x210 {
		t.Fatalf("process attributes = %#v, want hidden console and process-tree flags 0x210", command.SysProcAttr)
	}
}
