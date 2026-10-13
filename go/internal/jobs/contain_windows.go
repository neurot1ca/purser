//go:build windows

package jobs

import "purser-hook/internal/proc"

// Compatibility for durable callers; integration belongs to M5.
type ProcessJob = proc.ProcessJob

func NewProcessJob() (*ProcessJob, error)         { return proc.NewProcessJob() }
func ContainCurrentProcess() (*ProcessJob, error) { return proc.ContainCurrentProcess() }
