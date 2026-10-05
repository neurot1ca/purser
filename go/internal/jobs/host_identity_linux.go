//go:build linux

package jobs

import "os"

func hostProcessIdentity() (any, bool) {
	state, created := processIdentity(os.Getpid())
	if created == nil || state != ProcessAlive {
		return nil, false
	}
	return *created, true
}
