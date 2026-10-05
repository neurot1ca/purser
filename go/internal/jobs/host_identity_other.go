//go:build !windows && !linux && !darwin

package jobs

func hostProcessIdentity() (any, bool) { return nil, false }
