package main

import (
	"path/filepath"
	"testing"
)

func TestStateRootExpandsTildeAndKeepsDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PURSER_STATE_DIR", "~/custom-state")
	if got, want := stateRoot(), filepath.Join(home, "custom-state"); got != want {
		t.Fatalf("tilde state root = %q, want %q", got, want)
	}
	t.Setenv("PURSER_STATE_DIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "~/custom-config")
	if got, want := stateRoot(), filepath.Join(home, "custom-config", "purser"); got != want {
		t.Fatalf("tilde Claude config state root = %q, want %q", got, want)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got, want := stateRoot(), filepath.Join(home, ".claude", "purser"); got != want {
		t.Fatalf("default state root = %q, want %q", got, want)
	}
}
