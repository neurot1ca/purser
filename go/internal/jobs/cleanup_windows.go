//go:build windows

package jobs

import (
	"os"
	"syscall"
)

func isReparsePoint(fi os.FileInfo) bool {
	x, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && x.FileAttributes&0x400 != 0
}
