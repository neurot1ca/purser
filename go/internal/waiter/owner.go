package waiter

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Owner pins a session process against PID reuse. It never owns durable work.
type Owner struct {
	PID     int
	Created uint64
}

func (o Owner) String() string { return fmt.Sprintf("%d:%d", o.PID, o.Created) }
func ParseOwner(token string) (Owner, error) {
	parts := strings.Split(token, ":")
	if len(parts) != 2 {
		return Owner{}, fmt.Errorf("invalid session owner %q", token)
	}
	pid, e1 := strconv.Atoi(parts[0])
	created, e2 := strconv.ParseUint(parts[1], 10, 64)
	if e1 != nil || e2 != nil || pid <= 0 || pid > 1<<31-1 || created == 0 {
		return Owner{}, fmt.Errorf("invalid session owner %q", token)
	}
	return Owner{pid, created}, nil
}

func SessionOwner() (Owner, error) {
	if token := os.Getenv("PURSER_SESSION_OWNER"); token != "" {
		return ParseOwner(token)
	}
	pid := os.Getppid()
	child, err := PinOwner(os.Getpid())
	if err != nil {
		return Owner{}, err
	}
	for depth := 0; pid > 1 && depth < 64; depth++ {
		pinned, err := PinOwner(pid)
		if err != nil {
			return Owner{}, err
		}
		if pinned.Created > child.Created {
			return Owner{}, fmt.Errorf("reused session ancestor PID %d", pid)
		}
		parent, name, err := ownerParent(pid)
		if err != nil {
			return Owner{}, err
		}
		again, err := PinOwner(pid)
		if err != nil || again != pinned {
			return Owner{}, fmt.Errorf("session ancestry changed at %d", pid)
		}
		name = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), `\`, "/")
		command := strings.Fields(name)
		if len(command) > 0 && (filepath.Base(command[0]) == "claude" || filepath.Base(command[0]) == "claude.exe" ||
			((filepath.Base(command[0]) == "node" || filepath.Base(command[0]) == "node.exe") && strings.Contains(name, "@anthropic-ai/claude-code/"))) {
			return pinned, nil
		}
		if parent == pid {
			break
		}
		child = pinned
		pid = parent
	}
	return Owner{}, fmt.Errorf("Claude session owner not found in ancestry")
}
