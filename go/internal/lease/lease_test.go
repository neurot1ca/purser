package lease

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPythonLeaseRecords(t *testing.T) {
	valid := `{"version":1,"owners":{"active":{"state":"ACTIVE","scope":["src"],"task_id":"task"},"queued":{"state":"QUEUED","scope":["tests"],"task_id":"task2","job_id":"job","queued_at":1.5}},"conflicts":[{"path":"src/file","recorded_at":2}]}`
	if err := Validate([]byte(valid)); err != nil {
		t.Fatalf("valid Python-shaped record rejected: %v", err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"owner-state", `{"version":1,"owners":{"x":{"state":"BROKEN","scope":["a"],"task_id":"t"}}}`},
		{"queued-time", `{"version":1,"owners":{"x":{"state":"QUEUED","scope":["a"],"task_id":"t","job_id":"j","queued_at":"later"}}}`},
		{"empty-scope", `{"version":1,"owners":{"x":{"state":"ACTIVE","scope":[],"task_id":"t"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate([]byte(tc.raw)); err == nil {
				t.Fatal("malformed lease record accepted")
			}
		})
	}
}

func TestLockContentionAcrossProcesses(t *testing.T) {
	if path := os.Getenv("PURSER_LEASE_LOCK_HELPER"); path != "" {
		release, err := Lock(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".ready", []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
		if err := release(); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "scope_leases.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockContentionAcrossProcesses$")
	cmd.Env = append(os.Environ(), "PURSER_LEASE_LOCK_HELPER="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not acquire lease lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("second process acquired before holder released: %s", elapsed)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child process: %v", err)
	}
}
