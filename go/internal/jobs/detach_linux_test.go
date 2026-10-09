//go:build linux

package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLaunchDetachedLinux(t *testing.T) {
	if marker := os.Getenv("PURSER_DETACHED_TEST_MARKER"); marker != "" {
		cwd, _ := os.Getwd()
		data, _ := json.Marshal(map[string]string{
			"cwd": cwd, "value": os.Getenv("PURSER_DETACHED_TEST_VALUE"),
		})
		_ = os.WriteFile(marker, data, 0600)
		time.Sleep(time.Minute)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "child.json")
	env := append(os.Environ(), "PURSER_DETACHED_TEST_MARKER="+marker, "PURSER_DETACHED_TEST_VALUE=passed")
	pid, identity, err := LaunchDetached([]string{executable, "-test.run=^TestLaunchDetachedLinux$"}, env, dir)
	if err != nil {
		t.Fatalf("LaunchDetached: %v", err)
	}
	t.Cleanup(func() { _ = KillProcessGroup(pid) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("detached child did not write marker: %v", err)
	}
	var child map[string]string
	if err := json.Unmarshal(data, &child); err != nil {
		t.Fatal(err)
	}
	if child["cwd"] != dir || child["value"] != "passed" {
		t.Fatalf("detached child cwd/env = %#v", child)
	}
	if identity <= 0 || !IdentityMatches(pid, identity) {
		t.Fatalf("detached process identity did not match: pid=%d identity=%d", pid, identity)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil || group != pid {
		t.Fatalf("detached process group = %d, err=%v; want pid %d", group, err, pid)
	}
}
