//go:build windows

package waiter

import (
	"os"
	"strings"
	"testing"
)

func TestOwnerWindowsCommandLine(t *testing.T) {
	command, err := ownerCommandLine(os.Getpid())
	if err != nil || !strings.Contains(command, "-test.") {
		t.Fatalf("command line observation: %q %v", command, err)
	}
}
