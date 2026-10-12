package main

import (
	"os"
	"testing"

	"purser-hook/internal/jobs"
	"purser-hook/internal/testutil"
)

func TestMain(m *testing.M) {
	jobs.DisableApplyValidationForTests()
	_ = os.Setenv("PURSER_APPLY_VALIDATION_CHILD", "1")
	os.Exit(testutil.RunIsolated(m))
}
