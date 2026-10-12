package initsetup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAutoCompactOnWritesEnabled(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	var out, errOut bytes.Buffer
	if code := AutoCompactRun([]string{"on"}, &out, &errOut); code != 0 {
		t.Fatalf("AutoCompactRun returned %d: %s", code, errOut.String())
	}
	var marker struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(state, "auto-compact.json")), &marker); err != nil {
		t.Fatal(err)
	}
	if !marker.Enabled {
		t.Fatal("enabled = false, want true")
	}
}

func TestAutoCompactOffWritesDisabled(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	var out, errOut bytes.Buffer
	if code := AutoCompactRun([]string{"off"}, &out, &errOut); code != 0 {
		t.Fatalf("AutoCompactRun returned %d: %s", code, errOut.String())
	}
	var marker struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(state, "auto-compact.json")), &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Enabled {
		t.Fatal("enabled = true, want false")
	}
}

func TestAutoCompactStatusMissing(t *testing.T) {
	t.Setenv("PURSER_STATE_DIR", t.TempDir())
	var out, errOut bytes.Buffer
	if code := AutoCompactRun([]string{"status"}, &out, &errOut); code != 0 {
		t.Fatalf("AutoCompactRun returned %d: %s", code, errOut.String())
	}
	if got, want := out.String(), "{\"auto_compact\":\"not_set\"}\n"; got != want {
		t.Fatalf("acknowledgement = %q, want %q", got, want)
	}
}

func TestAutoCompactBadArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := AutoCompactRun([]string{"maybe"}, &out, &errOut); code != 2 {
		t.Fatalf("AutoCompactRun returned %d, want 2", code)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
