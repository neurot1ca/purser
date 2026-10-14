package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"purser-hook/internal/waiter"
	"time"
)

var registerSessionOwner = watchSessionOwner

func monitorSessionOwner(ctx context.Context, owner waiter.Owner, stderr io.Writer) (<-chan struct{}, error) {
	done := make(chan struct{})
	actual, err := waiter.PinOwner(owner.PID)
	if err == nil && actual != owner {
		close(done)
		return done, nil
	}
	watchCtx, stop := context.WithCancel(ctx)
	observed := make(chan struct{})
	if err == nil {
		err = registerSessionOwner(watchCtx, owner, observed, stderr)
	}
	if err != nil {
		stop()
		return nil, err
	}
	actual, err = waiter.PinOwner(owner.PID)
	if err != nil || actual != owner {
		stop()
		close(done)
		return done, nil
	}
	go func() {
		defer stop()
		defer close(done)
		select {
		case <-observed:
		case <-ctx.Done():
		}
	}()
	return done, nil
}

func sessionContext(stderr io.Writer) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	token := os.Getenv("PURSER_SESSION_OWNER")
	if token == "" {
		return ctx, cancel, nil
	}
	owner, err := waiter.ParseOwner(token)
	if err != nil {
		cancel()
		return ctx, cancel, err
	}
	done, err := monitorSessionOwner(ctx, owner, stderr)
	if err != nil {
		cancel()
		return ctx, cancel, err
	}
	select {
	case <-done:
		cancel()
		return ctx, cancel, nil
	default:
	}
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel, nil
}

// Identity polling backs up OS notifications, including Linux's creating-thread
// signal: only loss of the pinned actual owner ends the session.
func pollSessionOwner(ctx context.Context, owner waiter.Owner, done chan struct{}, stderr io.Writer) {
	defer close(done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		actual, err := waiter.PinOwner(owner.PID)
		if err != nil || actual != owner {
			if err != nil {
				fmt.Fprintf(stderr, "Purser: session owner observation ended: %v\n", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
