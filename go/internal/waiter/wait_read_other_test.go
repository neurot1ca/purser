//go:build !windows

package waiter_test

import "testing"

func holdJobRead(t *testing.T, path string) func() {
	t.Helper()
	t.Skip("transient sharing violations are covered on the Windows target")
	return func() {}
}
