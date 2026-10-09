package hookactivity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRecordFirstWrite(t *testing.T) {
	root := hookActivityTestRoot(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	Record(root, "PreToolUse", now)
	activity := Load(root)
	if got := activity["PreToolUse"]; !got.Equal(now) {
		t.Fatalf("PreToolUse timestamp = %s; want %s", got, now)
	}
	data, err := os.ReadFile(filepath.Join(root, fileName))
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["PreToolUse"] != now.Format(time.RFC3339) {
		t.Fatalf("stored timestamp = %q", stored["PreToolUse"])
	}
}

func TestRecordSkipsWithinHour(t *testing.T) {
	root := hookActivityTestRoot(t)
	first := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	Record(root, "Stop", first)
	path := filepath.Join(root, fileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	Record(root, "Stop", first.Add(59*time.Minute))
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("activity changed within the hour: before %s, after %s", before, after)
	}
}

func TestRecordRewritesAfterHour(t *testing.T) {
	root := hookActivityTestRoot(t)
	first := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	Record(root, "SessionStart", first)
	updated := first.Add(time.Hour)
	Record(root, "SessionStart", updated)
	if got := Load(root)["SessionStart"]; !got.Equal(updated) {
		t.Fatalf("SessionStart timestamp = %s; want %s", got, updated)
	}
}

func TestRecordIgnoresUnknownEvent(t *testing.T) {
	root := hookActivityTestRoot(t)
	Record(root, "WriteUndeliveredMarker", time.Now())
	if _, err := os.Stat(filepath.Join(root, fileName)); !os.IsNotExist(err) {
		t.Fatalf("unknown event wrote activity file; stat error = %v", err)
	}
}

func hookActivityTestRoot(t *testing.T) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", "..", ".tmp"))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "hookactivity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
