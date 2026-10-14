//go:build linux || darwin

package waiter

import (
	"fmt"
	"os/exec"
	"purser-hook/internal/proc"
	"strconv"
	"strings"
)

func PinOwner(pid int) (Owner, error) {
	state, created := proc.ProcessIdentity(pid)
	if state != proc.ProcessAlive || created == nil {
		return Owner{}, fmt.Errorf("session owner %d identity %s", pid, state)
	}
	return Owner{pid, *created}, nil
}
func ownerParent(pid int) (int, string, error) {
	command := proc.Hide(exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=", "-o", "args="))
	output, err := command.Output()
	if err != nil {
		return 0, "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("missing ancestry for %d", pid)
	}
	parent, err := strconv.Atoi(fields[0])
	return parent, strings.Join(fields[1:], " "), err
}
