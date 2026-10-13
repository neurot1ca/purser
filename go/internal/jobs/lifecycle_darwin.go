//go:build darwin

package jobs

import "purser-hook/internal/proc"

func processIdentity(pid int) (string, *uint64) { return proc.ProcessIdentity(pid) }
func ProcessGroupStopped(pid int) bool          { stopped, _ := proc.GroupStopped(pid); return stopped }
