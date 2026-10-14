//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"purser-hook/internal/waiter"
	"runtime"
	"syscall"
)

func watchSessionOwner(ctx context.Context, owner waiter.Owner, done chan struct{}, stderr io.Writer) error {
	// prctl applies to this thread. Keep it alive until shutdown. SIGUSR2 is
	// caught, never fatal; a thread-parent exit is merely an observation hint.
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGUSR2)
		defer signal.Stop(signals)
		parent := os.Getppid()
		pinned, err := waiter.PinOwner(parent)
		if err == nil {
			_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 1, uintptr(syscall.SIGUSR2), 0, 0, 0, 0)
			if errno != 0 {
				err = errno
			}
		}
		if err == nil {
			again, e := waiter.PinOwner(parent)
			if e != nil || again != pinned || os.Getppid() != parent {
				actual, ownerErr := waiter.PinOwner(owner.PID)
				if ownerErr != nil || actual != owner {
					err = fmt.Errorf("owner identity changed during death-signal registration")
				} else {
					fmt.Fprintln(stderr, "Purser: parent changed during death-signal registration; pinned session owner remains alive")
				}
			}
		}
		if err != nil {
			ready <- err
			return
		}
		ready <- nil
		// Polling also covers owner ancestors behind a resident fallback wrapper.
		pollSessionOwner(ctx, owner, done, stderr)
		syscall.RawSyscall6(syscall.SYS_PRCTL, 1, 0, 0, 0, 0, 0)
	}()
	return <-ready
}
