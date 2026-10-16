//go:build windows

package waiter_test

func processIdentityDiagnostic(int) (string, *uint64) {
	return "UNAVAILABLE", nil
}
