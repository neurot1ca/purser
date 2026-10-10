package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// RunIsolated gives a test binary private home and resolved temporary paths so
// path-derived fixture identities match the paths seen by production code.
func RunIsolated(m *testing.M) int {
	tempRoot, err := os.MkdirTemp("", "purser-tests-")
	if err != nil {
		return 1
	}
	defer os.RemoveAll(tempRoot)
	if resolved, resolveErr := filepath.EvalSymlinks(tempRoot); resolveErr == nil {
		tempRoot = resolved
	}
	home := filepath.Join(tempRoot, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		return 1
	}
	goCache := filepath.Join(tempRoot, "go-cache")
	if err := os.MkdirAll(goCache, 0700); err != nil {
		return 1
	}
	set := func(key, value string) bool { return os.Setenv(key, value) == nil }
	for _, key := range []string{"PURSER_STATE_DIR", "PURSER_CODEX_SHIM_STATE", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA"} {
		if err := os.Unsetenv(key); err != nil {
			return 1
		}
	}
	if !set("HOME", home) || !set("USERPROFILE", home) || !set("HOMEDRIVE", "") || !set("HOMEPATH", "") || !set("GOCACHE", goCache) {
		return 1
	}
	// testing.T.TempDir follows these variables, so point it at the physical
	// temp directory before any tests create paths for cross-language fixtures.
	canonicalTemp, err := filepath.EvalSymlinks(os.TempDir())
	if err == nil {
		for _, key := range []string{"TMP", "TEMP", "TMPDIR"} {
			if !set(key, canonicalTemp) {
				return 1
			}
		}
	}
	return m.Run()
}
