package review

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	stateDir, err := os.MkdirTemp("", "purser-review-tests-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("PURSER_STATE_DIR", stateDir); err != nil {
		_ = os.RemoveAll(stateDir)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(stateDir)
	os.Exit(code)
}
