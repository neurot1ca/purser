//go:build windows

package main

import "purser-hook/internal/jobs"

func containHost() (func(), error) {
	containment, err := jobs.ContainCurrentProcess()
	if err != nil {
		return nil, err
	}
	return func() { _ = containment.Close() }, nil
}
