//go:build !windows

package waiter_test

import "purser-hook/internal/proc"

func processIdentityDiagnostic(pid int) (string, *uint64) {
	return proc.ProcessIdentity(pid)
}
