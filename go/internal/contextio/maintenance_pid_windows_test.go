//go:build windows

package contextio

import (
	"os"
	"os/exec"
	"testing"
)

func TestMaintenancePidAlive(t *testing.T) {
	t.Run("current process", func(t *testing.T) {
		if !maintenancePIDAlive(os.Getpid()) {
			t.Fatalf("current process %d reported dead", os.Getpid())
		}
	})

	t.Run("exited process", func(t *testing.T) {
		cmd := exec.Command("cmd", "/c", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := cmd.Process.Pid
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if maintenancePIDAlive(pid) {
			t.Fatalf("exited process %d reported alive", pid)
		}
	})

	t.Run("nonexistent process", func(t *testing.T) {
		const pid = int(0xfffffffe)
		if maintenancePIDAlive(pid) {
			t.Fatalf("nonexistent process %d reported alive", pid)
		}
	})

	t.Run("system process access denied", func(t *testing.T) {
		if !maintenancePIDAlive(4) {
			t.Fatal("inaccessible system process should be treated as possibly alive")
		}
	})
}
