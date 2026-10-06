package learning

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestSourceKey(t *testing.T) {
	if got := SourceKey(strings.Repeat("a", 64)); got != strings.Repeat("a", 64) {
		t.Fatalf("valid digest changed: %q", got)
	}
	if got := SourceKey("job/α"); got != "opaque:f5d296779845e2ebfbf848eda1cac808c08b2a0b11052756f36284ebfa35d9b6" {
		t.Fatalf("opaque key = %q", got)
	}
}

func TestDistillNewThenSkipExisting(t *testing.T) {
	root := t.TempDir()
	src := fixtureSource(t, root, 1)
	ids, err := DistillJobs(root, []Source{src}, 100, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ids[SourceKey(strings.Repeat("0", 63)+"1")] || !IsDistilled(root, strings.Repeat("0", 63)+"1") {
		t.Fatal("new source was not checkpointed")
	}
	if _, err = DistillJobs(root, []Source{src}, 200, 10, nil); err != nil {
		t.Fatal(err)
	}
	d := ReadSummary(root)
	if len(d["records"].([]any)) != 1 {
		t.Fatalf("retry duplicated record: %d", len(d["records"].([]any)))
	}
	c := d["cohorts"].(map[string]any)["unknown|normal|model-a|high"].(map[string]any)
	if numberFloat(c["counts"].(map[string]any)["tasks"]) != 1 {
		t.Fatal("retry double-counted cohort")
	}
}

func TestDistillMaxSources(t *testing.T) {
	root := t.TempDir()
	sources := []Source{fixtureSource(t, root, 1), fixtureSource(t, root, 2), fixtureSource(t, root, 3)}
	ids, err := DistillJobs(root, sources, 100, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || len(ReadSummary(root)["records"].([]any)) != 2 {
		t.Fatalf("max_sources not applied: ids=%d records=%d", len(ids), len(ReadSummary(root)["records"].([]any)))
	}
}

func TestEncodeGoldenAndByteCapTrimming(t *testing.T) {
	data := goldenFixture()
	got, err := encode(data)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "encode.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Python _encode mismatch\n got: %s\nwant: %s", got, want)
	}
	large := empty()
	large["records"] = []any{map[string]any{"blob": strings.Repeat("x", MaxSummaryBytes+10)}}
	trimmed, err := encode(large)
	if err != nil {
		t.Fatal(err)
	}
	if len(trimmed) > MaxSummaryBytes || len(large["records"].([]any)) != 0 {
		t.Fatalf("oversize record was not trimmed: bytes=%d records=%d", len(trimmed), len(large["records"].([]any)))
	}
}

func TestReadCorruptAndOversized(t *testing.T) {
	root := t.TempDir()
	path := summaryPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, content := range [][]byte{[]byte("{"), bytes.Repeat([]byte("x"), MaxSummaryBytes+1)} {
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		got := ReadSummary(root)
		if got["schema_version"] != 1 || len(got["sources"].([]any)) != 0 {
			t.Fatalf("invalid summary not reset: %#v", got)
		}
	}
	valid := append([]byte(`{"schema_version":1,"sources":[],"records":[],"cohorts":{}}`), bytes.Repeat([]byte(" "), MaxSummaryBytes+1)...)
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if got := ReadSummary(root); numInt(got["schema_version"]) != 1 {
		t.Fatalf("valid oversized summary rejected: %#v", got)
	}
}

func TestPythonLockInterop(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "telemetry", LockName)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		t.Fatal(err)
	}
	var code string
	if runtime.GOOS == "windows" {
		code = "import msvcrt,sys,time; f=open(sys.argv[1],'a+b'); f.seek(0,2); f.write(b'0') if f.tell()==0 else None; f.flush(); f.seek(0); msvcrt.locking(f.fileno(),msvcrt.LK_LOCK,1); print('locked',flush=True); time.sleep(.35); f.seek(0); msvcrt.locking(f.fileno(),msvcrt.LK_UNLCK,1)"
	} else {
		code = "import fcntl,sys,time; f=open(sys.argv[1],'a+b'); f.seek(0,2); f.write(b'0') if f.tell()==0 else None; f.flush(); fcntl.flock(f,fcntl.LOCK_EX); print('locked',flush=True); time.sleep(.35); fcntl.flock(f,fcntl.LOCK_UN)"
	}
	cmd := exec.Command("python", "-c", code, lockPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Skipf("python unavailable: %v", err)
	}
	buf := make([]byte, 7)
	if _, err = stdout.Read(buf); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		unlock, e := contextio.LockFile(lockPath)
		if e == nil {
			e = unlock()
		}
		done <- e
	}()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("Go acquired Python-held lock immediately (%s)", time.Since(started))
	case <-time.After(120 * time.Millisecond):
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Go did not acquire lock after Python released it")
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func fixtureSource(t *testing.T, root string, n int) Source {
	t.Helper()
	id := strings.Repeat("0", 63) + string(rune('0'+n))
	path := filepath.Join(root, id+".json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return Source{Path: path, Job: map[string]any{"jobId": id, "taskId": "task", "state": "SUCCEEDED", "finishedAt": float64(20), "durationMs": float64(1000), "route": "normal", "jobModel": "model-a", "jobEffort": "high", "reviewerOutcome": "passed"}}
}

func goldenFixture() Summary {
	return Summary{"schema_version": 1, "sources": []any{strings.Repeat("a", 64)}, "records": []any{map[string]any{"id": strings.Repeat("a", 64), "task": "task-😀", "at": 1.25, "cohort": "proj|normal|m|high", "duration_ms": 1000, "acceptance": "accepted", "usage": map[string]any{"input_tokens": 7, "label": "é<&"}}}, "cohorts": map[string]any{"proj|normal|m|high": map[string]any{"counts": map[string]any{"tasks": 1, "acceptance": map[string]any{"accepted": 1}, "send_backs": nil, "review": map[string]any{}, "validation": map[string]any{}, "reviewer1_valid": nil, "reviewer1_false": nil, "apply_conflicts": map[string]any{}, "failures": map[string]any{}, "token_usage": map[string]any{}, "token_usage_complete": map[string]any{}}, "samples": []any{map[string]any{"at": 1.25, "duration_ms": 1000}}, "quota_samples": []any{}}}}
}
