package lease

import (
	"encoding/json"
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

func TestReleasePromotesQueuedOwnerLikePython(t *testing.T) {
	root := t.TempDir()
	input := `{"version":1,"owners":{"a":{"acquired_at":90,"scope":["src"],"state":"ACTIVE","task_id":"a"},"b":{"job_id":"job","queued_at":91,"scope":["docs"],"state":"QUEUED","task_id":"b"}}}`
	if err := os.WriteFile(filepath.Join(root, "scope_leases.json"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	got := New(root).Release("a", 100)
	if got != (Result{"OK", "LEASE_RELEASED"}) {
		t.Fatalf("release result: %#v", got)
	}
	raw, err := os.ReadFile(filepath.Join(root, "scope_leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"conflicts":[],"owners":{"a":{"acquired_at":90,"released_at":100,"scope":["src"],"state":"RELEASED","task_id":"a"},"b":{"acquired_at":100,"job_id":"job","queued_at":91,"scope":["docs"],"state":"ACTIVE","task_id":"b"}},"version":1}`
	if string(raw) != want {
		t.Fatalf("released Python fixture mismatch:\n got %s\nwant %s", raw, want)
	}
	if got := New(root).Release("b", 101); got.Code != "LEASE_RELEASED" {
		t.Fatalf("loaded mutation: %#v", got)
	}
	value, err := New(root).read()
	if err != nil || value["owners"].(map[string]any)["b"].(map[string]any)["released_at"] != json.Number("101") {
		t.Fatalf("mutated persisted lease: %#v, %v", value, err)
	}
}

func TestFailedReleaseWritePreservesPriorRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	prior := `{"version":1,"owners":{"a":{"state":"ACTIVE","scope":["src"],"task_id":"a"}}}`
	if err := os.WriteFile(path, []byte(prior), 0600); err != nil {
		t.Fatal(err)
	}
	oldReplace := replaceLeaseFile
	replaceLeaseFile = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { replaceLeaseFile = oldReplace })
	if got := New(root).Release("a", 100); got.Code != "LEASE_RELEASE_UNCERTAIN" {
		t.Fatalf("failed mutation result: %#v", got)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != prior {
		t.Fatalf("prior record not recoverable: err=%v content=%s", err, after)
	}
}

func TestPromoteThenReleaseReadsFloatTimestampsInMemory(t *testing.T) {
	store := New(t.TempDir())
	value := map[string]any{"owners": map[string]any{
		"a": map[string]any{"state": "ACTIVE", "scope": []any{"src"}, "task_id": "a"},
		"b": map[string]any{"state": "QUEUED", "scope": []any{"docs"}, "task_id": "b", "job_id": "j", "queued_at": json.Number("1")},
	}}
	if !store.promote(value, 2) {
		t.Fatal("queued owner was not promoted")
	}
	if changed, missing := store.releaseOwner(value, "a", 3); !changed || missing {
		t.Fatalf("release: %v %v", changed, missing)
	}
	owners := value["owners"].(map[string]any)
	if n, ok := pythonFloat(owners["b"].(map[string]any)["acquired_at"]); !ok || n != 2 {
		t.Fatalf("promoted float timestamp rejected: %v %v", n, ok)
	}
	if n, ok := pythonFloat(owners["a"].(map[string]any)["released_at"]); !ok || n != 3 {
		t.Fatalf("released float timestamp rejected: %v %v", n, ok)
	}
}

func TestHotPathPromotionHonorsConflictOverlap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	fixture := `{"version":1,"conflicts":[{"path":"src/sub/file","recorded_at":90},{"path":"stale","recorded_at":1}],"owners":{"active":{"state":"ACTIVE","scope":["src/sub"],"task_id":"a","acquired_at":80},"old":{"state":"RELEASED","scope":["old"],"task_id":"old","released_at":1},"queued":{"state":"QUEUED","scope":["src"],"task_id":"b","job_id":"j","queued_at":85,"hot_path_queue":true}}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := New(root).Reconcile(100)
	if err != nil || changed {
		t.Fatalf("blocked hot-path promotion: changed=%v err=%v", changed, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != fixture {
		t.Fatalf("no-transition reconcile pruned data: %v data=%s", readErr, after)
	}
}
