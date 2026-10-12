package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"purser-hook/internal/testutil"
)

func TestMain(m *testing.M) {
	DisableApplyValidationForTests()
	applyValidationExecutable = func() (string, error) { return "purser-hook", nil }
	if os.Getenv("PURSER_HOST_FAKE_WORKER") == "1" {
		os.Exit(m.Run())
	}
	os.Exit(testutil.RunIsolated(m))
}

func jobsScratchDir(t *testing.T, pattern string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".tmp"))
	if err != nil || os.MkdirAll(root, 0700) != nil {
		t.Fatal("create repository test scratch directory")
	}
	dir, err := os.MkdirTemp(root, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
