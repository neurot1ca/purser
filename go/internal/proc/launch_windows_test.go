//go:build windows

package proc

import (
	"os/exec"
	"testing"
)

func TestConfigureBelowNormalPriorityPreservesNoWindow(t *testing.T) {
	command := exec.Command("git")
	ConfigureProcessTreeKill(command)
	ConfigureBelowNormalPriority(command)

	if command.SysProcAttr.CreationFlags != 0x08004200 || !command.SysProcAttr.HideWindow {
		t.Fatalf("process attributes = %#v, want flags 0x08004200 and hidden window", command.SysProcAttr)
	}
}

func TestConfigureBelowNormalPriorityAlone(t *testing.T) {
	command := exec.Command("git")
	ConfigureBelowNormalPriority(command)

	if command.SysProcAttr == nil || command.SysProcAttr.CreationFlags != 0x00004000 {
		t.Fatalf("process attributes = %#v, want flags 0x00004000", command.SysProcAttr)
	}
}
