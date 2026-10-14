//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"purser-hook/internal/waiter"
	"syscall"
)

func watchSessionOwner(ctx context.Context, owner waiter.Owner, done chan struct{}, stderr io.Writer) error {
	handle, err := waiter.OpenOwner(owner)
	if err != nil {
		return err
	}
	go func() {
		defer syscall.CloseHandle(handle)
		defer close(done)
		for ctx.Err() == nil {
			result, err := syscall.WaitForSingleObject(handle, 100)
			if err != nil {
				fmt.Fprintf(stderr, "Purser: session owner monitor wait failed: %v\n", err)
				return
			}
			if result == 0 {
				return
			}
			if result != 258 {
				fmt.Fprintf(stderr, "Purser: session owner monitor unexpected wait: %d\n", result)
				return
			}
		}
	}()
	return nil
}
