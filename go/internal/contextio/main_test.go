package contextio_test

import (
	"os"
	"testing"

	"purser-hook/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunIsolated(m))
}
