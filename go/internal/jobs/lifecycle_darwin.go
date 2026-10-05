//go:build darwin

package jobs

import "syscall"

// Darwin intentionally has no host identity proof, matching the bridge's
// conservative UNCONFIRMED classification.
func processIdentity(int) (string, *uint64) {
	return ProcessUnconfirmed, nil
}

func ProcessGroupStopped(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == syscall.ESRCH
}
