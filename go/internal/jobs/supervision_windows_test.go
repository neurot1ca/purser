//go:build windows

package jobs

func hostLeafIdentity(pid int) (int64, bool) {
	identity, alive, err := processIdentity(uint32(pid))
	return identity, err == nil && alive
}
