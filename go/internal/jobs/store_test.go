package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	fixtures := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "codex_exec_records")
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

func TestPythonOwnerLockInteroperability(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("python unavailable for lock interoperability")
	}
	dir := scratch(t)
	fixtureLock := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "codex_exec_records", "interrupted-host.json")
	b, err := os.ReadFile(fixtureLock)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Files map[string]string `json:"files"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	var rel, content string
	for name, value := range fixture.Files {
		if strings.HasSuffix(name, ".owner-lock") {
			rel, content = name, value
			break
		}
	}
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	// msvcrt.locking requires the lock byte to exist; the fixture itself is empty.
	if runtime.GOOS == "windows" {
		if err = os.WriteFile(path, []byte("0"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := AcquireOwnership(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".owner-lock"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" { // msvcrt.locking must fail while Go holds the same byte range.
		cmd := exec.Command(python, "-c", `import msvcrt,sys; f=open(sys.argv[1],"a+b"); f.seek(0);
try: msvcrt.locking(f.fileno(),msvcrt.LK_NBLCK,1); sys.exit(2)
except OSError: sys.exit(0)`, path)
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			lock.Release()
			t.Fatalf("Python did not observe Go lock: %v: %s", runErr, output)
		}
	} else {
		cmd := exec.Command(python, "-c", `import fcntl,sys; f=open(sys.argv[1],"a+b");
try: fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB); sys.exit(2)
except BlockingIOError: sys.exit(0)`, path)
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			lock.Release()
			t.Fatalf("Python did not observe Go lock: %v: %s", runErr, output)
		}
	}
	if err = lock.Release(); err != nil {
		t.Fatal(err)
	}
	// Python takes the fixture lock; the Go acquire must wait and then proceed.
	script := `import fcntl,sys,time; f=open(sys.argv[1],"a+b"); fcntl.flock(f,fcntl.LOCK_EX); print("locked",flush=True); time.sleep(.15)`
	if runtime.GOOS == "windows" {
		script = `import msvcrt,sys,time; f=open(sys.argv[1],"a+b"); f.seek(0); msvcrt.locking(f.fileno(),msvcrt.LK_LOCK,1); print("locked",flush=True); time.sleep(.15)`
	}
	cmd := exec.Command(python, "-c", script, path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := make([]byte, 7)
	if _, err = stdout.Read(line); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		l, e := AcquireOwnership(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".owner-lock"))
		if e == nil {
			e = l.Release()
		}
		acquired <- e
	}()
	select {
	case err = <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Go lock did not resume after Python released")
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
