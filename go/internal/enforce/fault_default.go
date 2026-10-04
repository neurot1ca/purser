//go:build !contracttest

package enforce

func contractFault() error { return nil }

func postFault() error { return nil }
