//go:build linux || darwin

package jobs

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestDetachedProcessGroupCanBeKilledAndConfirmedStopped(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exec sleep 60")
	ConfigureDetachedSession(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := KillProcessGroup(pid); err != nil {
		t.Fatalf("kill process group: %v", err)
	}
	if err := command.Wait(); err != nil {
		// SIGKILL is the expected command result.
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("wait after kill: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for !ProcessGroupStopped(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ProcessGroupStopped(pid) {
		t.Fatal("process group did not reach stopped state")
	}
}

func TestPlatformProcessIdentityContract(t *testing.T) {
	if runtime.GOOS == "darwin" {
		if got := ClassifyProcessIdentity(1, 1); got != ProcessUnconfirmed {
			t.Fatalf("Darwin identity = %s, want %s", got, ProcessUnconfirmed)
		}
		return
	}
	command := exec.Command("/bin/sh", "-c", "exit 0")
	ConfigureDetachedSession(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := ClassifyProcessIdentity(pid, 1); got != ProcessDead {
		t.Fatalf("identity after exit = %s, want %s", got, ProcessDead)
	}
	if runtime.GOOS == "linux" {
		state, start := processIdentity(os.Getpid())
		if state != ProcessAlive || start == nil {
			t.Fatalf("current process identity unavailable: state=%s", state)
		}
		if got := ClassifyProcessIdentity(os.Getpid(), *start+1); got != ProcessDead {
			t.Fatalf("mismatched identity = %s, want %s", got, ProcessDead)
		}
	}
}
