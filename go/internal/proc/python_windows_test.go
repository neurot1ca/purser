//go:build windows

package proc

import (
	"context"
	"testing"
)

func TestPythonLocatorLaunchIsHidden(t *testing.T) {
	command := pythonLocatorCommand(context.Background(), "node.exe", "python-locator.js")
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow {
		t.Fatal("Python locator launch does not hide its Windows window")
	}
	if command.SysProcAttr.CreationFlags&0x08000000 == 0 {
		t.Fatalf("Python locator creation flags %#x omit CREATE_NO_WINDOW", command.SysProcAttr.CreationFlags)
	}
}
