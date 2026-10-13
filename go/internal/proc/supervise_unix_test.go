//go:build linux || darwin

package proc

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestSupervisionDeniedOperations(t *testing.T) {
	originalKill, originalObserve := killSupervisedGroup, observeSupervisedGroup
	defer func() { killSupervisedGroup, observeSupervisedGroup = originalKill, originalObserve }()
	// Permission denial is deterministic here; native helpers exercise real
	// group killing. Do not require privileged users or fixture processes.
	killSupervisedGroup = func(int, syscall.Signal) error { return syscall.EPERM }
	confirmed, err := stopAnchoredGroup(&exec.Cmd{Process: &os.Process{Pid: 123}})
	if confirmed || err != syscall.EPERM {
		t.Fatalf("kill = %t, %v", confirmed, err)
	}
	killSupervisedGroup = originalKill
	command := Hide(exec.Command(os.Args[0], "-test.run=TestSupervisionHelper", "--", "worker", "leaf", ""))
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	Hide(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	observeSupervisedGroup = func(int) (bool, error) { return false, syscall.EACCES }
	confirmed, err = stopAnchoredGroup(command)
	if confirmed || err != syscall.EACCES {
		t.Fatalf("query = %t, %v", confirmed, err)
	}
	if eventError(supervisionEvent{Errno: uint64(syscall.EPERM)}) != syscall.EPERM {
		t.Fatal("exact denial lost")
	}
}
