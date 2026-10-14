package main

import (
	"os"
	"strings"
	"testing"

	"purser-hook/internal/jobs"
	"purser-hook/internal/testutil"
)

func TestMain(m *testing.M) {
	requestCleanup = func(string) error { return nil }
	jobs.DisableApplyValidationForTests()
	_ = os.Setenv("PURSER_APPLY_VALIDATION_CHILD", "1")
	os.Exit(testutil.RunIsolated(m))
}

func TestCleanupLaunchRefusesTestBinary(t *testing.T) {
	oldLaunch := launchPrivateChild
	launches := 0
	launchPrivateChild = func([]string, []string, string) (int, int64, error) {
		launches++
		return 42, 7, nil
	}
	t.Cleanup(func() { launchPrivateChild = oldLaunch })
	root := t.TempDir()
	pid, identity, err := launchPrivate(root, "cleanup", root)
	if err == nil || !strings.Contains(err.Error(), "test binary") || pid != 0 || identity != 0 || launches != 0 {
		t.Fatalf("cleanup launch: pid=%d identity=%d err=%v launches=%d", pid, identity, err, launches)
	}
	allowCleanupTestChild = true
	t.Cleanup(func() { allowCleanupTestChild = false })
	pid, identity, err = launchPrivate(root, "cleanup", root)
	if err != nil || pid != 42 || identity != 7 || launches != 1 {
		t.Fatalf("opted-in cleanup launch: pid=%d identity=%d err=%v launches=%d", pid, identity, err, launches)
	}
}
