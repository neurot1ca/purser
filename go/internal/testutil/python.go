package testutil

import (
	"os/exec"
	"testing"
)

// Python returns the first Python interpreter on PATH: python, then python3.
func Python(t testing.TB) string {
	t.Helper()
	for _, name := range []string{"python", "python3"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Fatal("Python interpreter not found (tried python and python3)")
	return ""
}
