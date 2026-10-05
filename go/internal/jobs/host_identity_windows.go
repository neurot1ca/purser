//go:build windows

package jobs

import "os"

func hostProcessIdentity() (any, bool) {
	created, alive, err := processIdentity(uint32(os.Getpid()))
	return created, err == nil && alive
}
