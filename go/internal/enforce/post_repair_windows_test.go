//go:build windows

package enforce

import (
	"context"
	"testing"
)

func TestPostRepairCommandHidesConsole(t *testing.T) {
	command := newPostRepairCommand(context.Background(), "codex", "exec", "resume")
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags != 0x08000200 {
		t.Fatalf("process attributes = %#v, want CREATE_NO_WINDOW, hidden window, and process-tree flags", command.SysProcAttr)
	}
}
