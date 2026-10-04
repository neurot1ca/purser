//go:build contracttest

package enforce

import (
	"errors"
	"os"
)

func contractFault() error {
	if os.Getenv("PURSER_CONTRACT_SEAM") == "config-error" {
		return errors.New("injected config failure")
	}
	return nil
}

func postFault() error {
	if os.Getenv("PURSER_CONTRACT_SEAM") == "post-error" {
		return errors.New("injected post failure")
	}
	return nil
}
