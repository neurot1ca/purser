//go:build darwin

package main

import (
	"context"
	"fmt"
	"io"
	"purser-hook/internal/waiter"
	"syscall"
)

func watchSessionOwner(ctx context.Context, owner waiter.Owner, done chan struct{}, stderr io.Writer) error {
	fd, err := syscall.Kqueue()
	if err == nil {
		event := syscall.Kevent_t{Ident: uint64(owner.PID), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ENABLE | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT}
		_, err = syscall.Kevent(fd, []syscall.Kevent_t{event}, nil, nil)
	}
	if err != nil {
		if fd >= 0 {
			syscall.Close(fd)
		}
		fmt.Fprintf(stderr, "Purser: session owner monitor kqueue unavailable; polling: %v\n", err)
		go pollSessionOwner(ctx, owner, done, stderr)
		return nil
	}
	// Registration precedes the second identity observation, closing PID-reuse
	// and already-exited-owner races without relying on a future NOTE_EXIT.
	actual, checkErr := waiter.PinOwner(owner.PID)
	if checkErr != nil || actual != owner {
		syscall.Close(fd)
		close(done)
		return nil
	}
	go func() {
		defer syscall.Close(fd)
		defer close(done)
		events := make([]syscall.Kevent_t, 1)
		for ctx.Err() == nil {
			timeout := syscall.Timespec{Nsec: 100000000}
			n, err := syscall.Kevent(fd, nil, events, &timeout)
			if err == nil && n > 0 && events[0].Flags&syscall.EV_ERROR != 0 {
				err = syscall.Errno(events[0].Data)
			}
			if err != nil {
				fmt.Fprintf(stderr, "Purser: session owner monitor kqueue wait failed; polling: %v\n", err)
				fallback := make(chan struct{})
				pollSessionOwner(ctx, owner, fallback, stderr)
				return
			}
			if n > 0 {
				return
			}
			actual, err := waiter.PinOwner(owner.PID)
			if err != nil || actual != owner {
				return
			}
		}
	}()
	return nil
}
