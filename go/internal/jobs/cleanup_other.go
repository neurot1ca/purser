//go:build !windows

package jobs

import "os"

func isReparsePoint(os.FileInfo) bool { return false }
