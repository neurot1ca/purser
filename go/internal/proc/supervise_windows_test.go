//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestSupervisionDeniedOperations(t *testing.T) {
	containment, _, err := newSupervisionContainment()
	if err != nil {
		t.Fatal(err)
	}
	defer containment.close()
	current, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	// A query-only job handle cannot assign or terminate processes.
	var h syscall.Handle
	err = syscall.DuplicateHandle(current, containment.job.handle, current, &h, 4, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	restricted := &ProcessJob{handle: h}
	defer restricted.Close()
	if err := restricted.Assign(os.Getpid()); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("assignment: %v", err)
	}
	confirmed, err := (supervisionContainment{job: restricted}).stop()
	if confirmed || !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("termination = %t, %v", confirmed, err)
	}
	// A terminate-only handle cannot query accounting.
	err = syscall.DuplicateHandle(current, containment.job.handle, current, &h, 8, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	queryDenied := &ProcessJob{handle: syscall.Handle(h)}
	defer queryDenied.Close()
	confirmed, err = queryDenied.ConfirmStopped(time.Second)
	if confirmed || !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("query = %t, %v", confirmed, err)
	}
	e := supervisionEvent{Errno: uint64(syscall.ERROR_ACCESS_DENIED), Error: syscall.ERROR_ACCESS_DENIED.Error()}
	if eventError(e) != syscall.ERROR_ACCESS_DENIED {
		t.Fatalf("protocol changed exact denial: %v", eventError(e))
	}
}

func TestSupervisionHiddenConsole(t *testing.T) {
	command := exec.Command("unused")
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000008 | 0x200}
	configureGuard(command)
	Hide(command)
	if !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags != 0x210 {
		t.Fatalf("final attributes: %+v", command.SysProcAttr)
	}
}
