//go:build windows

package proc

import "purser-hook/internal/testutil"

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestHideDisablesDescendantAutoUpdate(t *testing.T) {
	t.Setenv("AGY_CLI_DISABLE_AUTO_UPDATE", "1")
	for _, env := range [][]string{nil, {"agy_cli_disable_auto_update=false", "PURSER_KEEP=value"}} {
		command := exec.Command("agy.exe")
		command.Env = env
		Hide(command)
		Hide(command) // Guards and workers can apply launch attributes more than once.
		value := ""
		for _, entry := range command.Env {
			key, v, _ := strings.Cut(entry, "=")
			if strings.EqualFold(key, "AGY_CLI_DISABLE_AUTO_UPDATE") {
				value = v // os/exec uses the last value for duplicate Windows keys.
			}
		}
		if value != "true" {
			t.Fatalf("auto-update opt-out = %q, want true", value)
		}
		if env != nil && !strings.Contains(strings.Join(command.Env, "\n"), "PURSER_KEEP=value") {
			t.Fatal("lost explicit environment")
		}
		if !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags != createNoWindow {
			t.Fatalf("process attributes = %#v", command.SysProcAttr)
		}
	}
}

func TestHideMergesProcessAttributes(t *testing.T) {
	command := testutil.Hide(exec.Command("git"))
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x200}

	if got := Hide(command); got != command {
		t.Fatal("Hide did not return the command")
	}
	if command.SysProcAttr.CreationFlags != 0x08000200 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want CREATE_NO_WINDOW, preserved flags, and hidden window", command.SysProcAttr)
	}
}

func TestHideClearsIncompatibleConsoleFlags(t *testing.T) {
	command := exec.Command("git")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08004018}

	Hide(command)

	flags := command.SysProcAttr.CreationFlags
	if flags != 0x08004000 || flags&0x10 != 0 || flags&0x8 != 0 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want CREATE_NO_WINDOW, no CREATE_NEW_CONSOLE or DETACHED_PROCESS, and hidden window", command.SysProcAttr)
	}
}
