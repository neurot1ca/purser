//go:build !windows && !linux && !darwin

package jobs

import "errors"

// LaunchDetached is implemented by the platform lifecycle leaf.
func LaunchDetached(_ []string, _ []string, _ string) (int, int64, error) {
	return 0, 0, errors.New("detached host launch is not implemented on this platform")
}

// IdentityMatches cannot prove process identity on this platform in this pass.
func IdentityMatches(_ int, _ int64) bool { return false }
