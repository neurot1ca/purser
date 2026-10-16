//go:build linux

package proc

import (
	"syscall"
	"testing"
)

func TestProcEntryVanished(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "no such file", err: syscall.ENOENT, want: true},
		{name: "process exited", err: syscall.ESRCH, want: true},
		{name: "real error", err: syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := procEntryVanished(tc.err); got != tc.want {
				t.Fatalf("procEntryVanished(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}
