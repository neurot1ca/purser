package jobs

import (
	"os"
	"testing"

	"purser-hook/internal/testutil"
)

func TestMain(m *testing.M) {
	if os.Getenv("PURSER_HOST_FAKE_WORKER") == "1" {
		os.Exit(m.Run())
	}
	os.Exit(testutil.RunIsolated(m))
}
