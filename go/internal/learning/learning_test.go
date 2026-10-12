package learning

import "purser-hook/internal/testutil"

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func TestDistillOverflowCaps(t *testing.T) {
	root := t.TempDir()
	sources := make([]Source, 1200)
	for i := range sources {
		id := fmt.Sprintf("%064x", i+1)
		path := filepath.Join(root, id+".json")
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		sources[i] = Source{Path: path, Job: map[string]any{
			"jobId": id, "taskId": fmt.Sprintf("task-%d", i), "state": "SUCCEEDED",
			"finishedAt": float64(20), "durationMs": float64(1000), "route": fmt.Sprintf("route-%d", i),
			"jobModel": "model-a", "jobEffort": "high", "reviewerOutcome": "passed",
		}}
	}
	for i := 0; i < 3; i++ {
		if _, err := DistillJobs(root, sources, 100, MaxDistillPerRun, nil); err != nil {
			t.Fatal(err)
		}
	}

	data := ReadSummary(root)
	records := data["records"].([]any)
	if len(records) > MaxTaskRecords {
		t.Fatalf("record cap exceeded: %d", len(records))
	}
	for i, value := range records {
		if got := len(pythonBytes(value)); got > MaxTaskBytes {
			t.Fatalf("record %d is %d bytes, cap %d", i, got, MaxTaskBytes)
		}
	}
	cohorts := data["cohorts"].(map[string]any)
	if len(cohorts) > MaxCohorts {
		t.Fatalf("cohort cap exceeded: %d", len(cohorts))
	}
	if _, ok := cohorts["other"]; !ok {
		t.Fatal("overflow cohort was not aggregated into other")
	}
	for name, value := range cohorts {
		if got := len(value.(map[string]any)["samples"].([]any)); got > MaxSamples {
			t.Errorf("cohort %q has %d samples, cap %d", name, got, MaxSamples)
		}
	}
	info, err := os.Stat(summaryPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > MaxSummaryBytes {
		t.Fatalf("summary is %d bytes, cap %d", info.Size(), MaxSummaryBytes)
	}
}

func TestDistillCheckpointRetryDoesNotDoubleCount(t *testing.T) {
	root := t.TempDir()
	src := fixtureSource(t, root, 1)
	telemetry := filepath.Join(root, "telemetry")
	if err := os.WriteFile(telemetry, []byte("blocking directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DistillJobs(root, []Source{src}, 100, 10, nil); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	if err := os.Remove(telemetry); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := DistillJobs(root, []Source{src}, 100, 10, nil); err != nil {
			t.Fatal(err)
		}
	}
	counts := ReadSummary(root)["cohorts"].(map[string]any)["unknown|normal|model-a|high"].(map[string]any)["counts"].(map[string]any)
	if got := numberFloat(counts["tasks"]); got != 1 {
		t.Fatalf("checkpoint retry counted task %.0f times", got)
	}
}

func TestDistillReviewUnavailableAndSkippedAreDistinct(t *testing.T) {
	root := t.TempDir()
	unavailable := fixtureSource(t, root, 1)
	skipped := fixtureSource(t, root, 2)
	unavailable.Job["reviewerOutcome"] = "unavailable"
	skipped.Job["reviewerOutcome"] = "skipped"
	if _, err := DistillJobs(root, []Source{unavailable, skipped}, 100, 10, nil); err != nil {
		t.Fatal(err)
	}
	cohorts := ReadSummary(root)["cohorts"].(map[string]any)
	counts := cohorts["unknown|normal|model-a|high"].(map[string]any)["counts"].(map[string]any)
	review := counts["review"].(map[string]any)
	if numberFloat(review["passed"]) != 0 || numberFloat(review["unavailable"]) != 1 || numberFloat(review["skipped"]) != 1 {
		t.Fatalf("review outcomes = %#v", review)
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

func TestLockFileInterop(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "telemetry", LockName)
	cmd := testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestLockFileInteropChild$", "-test.v"))
	cmd.Env = append(os.Environ(), "PURSER_LOCK_INTEROP_PATH="+lockPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	locked := false
	for scanner.Scan() {
		if scanner.Text() == "locked" {
			locked = true
			break
		}
	}
	if !locked {
		t.Fatalf("child did not announce lock acquisition: %v", scanner.Err())
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
		t.Fatal("Go did not acquire lock after child released it")
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLockFileInteropChild(t *testing.T) {
	path := os.Getenv("PURSER_LOCK_INTEROP_PATH")
	if path == "" {
		return
	}
	unlock, err := contextio.LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	time.Sleep(350 * time.Millisecond)
	if err := unlock(); err != nil {
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
