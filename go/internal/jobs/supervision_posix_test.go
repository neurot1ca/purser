//go:build linux || darwin

package jobs

func hostLeafIdentity(pid int) (int64, bool) {
	state, identity := processIdentity(pid)
	if identity == nil {
		return 0, state != ProcessDead
	}
	return int64(*identity), state != ProcessDead
}
