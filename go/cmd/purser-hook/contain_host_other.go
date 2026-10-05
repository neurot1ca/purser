//go:build !windows

package main

func containHost() (func(), error) { return nil, nil }
