package jobs

import "purser-hook/internal/testutil"

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func scratch(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "jobs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestFixtureRecordsRoundTripBytes(t *testing.T) {
	fixtures := filepath.Join("testdata", "codex_exec_records")
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	root := scratch(t)
	for _, entry := range entries {
		b, err := os.ReadFile(filepath.Join(fixtures, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var fixture struct {
			Files map[string]string `json:"files"`
		}
		if err = json.Unmarshal(b, &fixture); err != nil {
			t.Fatal(err)
		}
		for name, content := range fixture.Files {
			if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".launch") {
				continue
			}
			got, err := Decode([]byte(content))
			if err != nil {
				t.Fatalf("%s/%s: %v", entry.Name(), name, err)
			}
			path := filepath.Join(root, filepath.FromSlash(name))
			if err = Write(path, got); err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != content {
				t.Fatalf("%s/%s changed bytes\nwant %s\n got %s", entry.Name(), name, content, actual)
			}
		}
	}
}

func TestUnknownFieldsAndPythonEscaping(t *testing.T) {
	r, err := Decode([]byte(`{"first": 1, "unknown": {"kept": true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Set("first", "<café>& "); err != nil {
		t.Fatal(err)
	}
	b, err := r.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"first": "<café>& ", "unknown": {"kept": true}}`
	if string(b) != want {
		t.Fatalf("want %s, got %s", want, b)
	}
}

func TestInterruptedReplaceLeavesOldRecord(t *testing.T) {
	path := filepath.Join(scratch(t), "job.json")
	old := []byte(`{"state": "QUEUED"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	r, _ := Decode([]byte(`{"state": "FAILED"}`))
	prior := replaceFile
	replaceFile = func(string, string) error { return errors.New("interrupted replace") }
	t.Cleanup(func() { replaceFile = prior })
	if err := Write(path, r); err == nil {
		t.Fatal("expected replace error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("old record lost: %s", got)
	}
}

func TestAtomicWriteRetriesTransientReplaceError(t *testing.T) {
	path := filepath.Join(scratch(t), "job.json")
	record, err := Decode([]byte(`{"state": "RUNNING"}`))
	if err != nil {
		t.Fatal(err)
	}
	prior := replaceFile
	attempts := 0
	replaceFile = func(source, target string) error {
		if target == path && attempts < 2 {
			attempts++
			return syscall.EACCES
		}
		return prior(source, target)
	}
	t.Cleanup(func() { replaceFile = prior })
	if err := Write(path, record); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("transient replace attempts = %d, want 2", attempts)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if value := recordMap(got)["state"]; value != "RUNNING" {
		t.Fatalf("written state = %v, want RUNNING", value)
	}
}

func TestAtomicWriteRetriesAccessDeniedForTransientWindow(t *testing.T) {
	path := filepath.Join(scratch(t), "job.json")
	prior := replaceFile
	started := time.Now()
	accessDeniedFor := 500 * time.Millisecond
	attempts := 0
	replaceFile = func(source, target string) error {
		if target == path && time.Since(started) < accessDeniedFor {
			attempts++
			return syscall.EACCES
		}
		return prior(source, target)
	}
	t.Cleanup(func() { replaceFile = prior })
	if err := atomicWrite(path, []byte(`{"state":"RUNNING"}`)); err != nil {
		t.Fatalf("atomicWrite after transient access denied: %v", err)
	}
	if attempts == 0 {
		t.Fatal("replace stub did not return access denied")
	}
}

func TestAtomicWriteReturnsImmediatelyForNonTransientReplaceError(t *testing.T) {
	path := filepath.Join(scratch(t), "job.json")
	want := errors.New("permanent replacement failure")
	prior := replaceFile
	attempts := 0
	replaceFile = func(string, string) error {
		attempts++
		return want
	}
	t.Cleanup(func() { replaceFile = prior })
	started := time.Now()
	err := atomicWrite(path, []byte(`{"state":"RUNNING"}`))
	if !errors.Is(err, want) {
		t.Fatalf("atomicWrite error = %v, want %v", err, want)
	}
	if attempts != 1 {
		t.Fatalf("replace attempts = %d, want 1", attempts)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("non-transient replacement took %s, want immediate return", elapsed)
	}
}

func TestConcurrentGenerationClaimsAreFenced(t *testing.T) {
	path := filepath.Join(scratch(t), strings.Repeat("a", 64)+".json")
	if err := os.WriteFile(path, []byte(`{"state": "QUEUED", "generation": 4, "ownershipToken": "owner"}`), 0600); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; _, err := ClaimGeneration(path, 4, "owner"); results <- err }()
	}
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("want one winner: %v, %v", a, b)
	}
	loser := a
	if loser == nil {
		loser = b
	}
	var conflict *ClaimConflictError
	if !errors.Is(loser, ErrClaimConflict) || !errors.As(loser, &conflict) {
		t.Fatalf("loser error is not typed: %v", loser)
	}
	stale, _ := Decode([]byte(`{"state": "FAILED", "generation": 3, "ownershipToken": "owner"}`))
	if err := WriteFenced(path, stale, 3, "owner"); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale writer accepted: %v", err)
	}
}

func TestOwnershipLockSubprocessHelper(t *testing.T) {
	if os.Getenv("PURSER_LOCK_HELPER") != "1" {
		return
	}
	dir := os.Getenv("PURSER_LOCK_DIR")
	started := os.Getenv("PURSER_LOCK_STARTED")
	acquired := os.Getenv("PURSER_LOCK_ACQUIRED")
	if err := os.WriteFile(started, []byte("started"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireOwnership(dir, "job")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(acquired, []byte("acquired"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipLockSerializesGoProcesses(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireOwnership(dir, "job")
	if err != nil {
		t.Fatal(err)
	}
	started, acquired := filepath.Join(dir, "started"), filepath.Join(dir, "acquired")
	cmd := testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestOwnershipLockSubprocessHelper$"))
	cmd.Env = append(os.Environ(), "PURSER_LOCK_HELPER=1", "PURSER_LOCK_DIR="+dir, "PURSER_LOCK_STARTED="+started, "PURSER_LOCK_ACQUIRED="+acquired)
	if err := cmd.Start(); err != nil {
		_ = lock.Release()
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		_ = lock.Release()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("lock helper did not start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(acquired); err == nil {
		_ = lock.Release()
		_ = cmd.Wait()
		t.Fatal("second process acquired an owned job lock")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acquired); err != nil {
		t.Fatalf("waiting process did not acquire released lock: %v", err)
	}
}
