package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestJournalWriteRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "turn.json")
	turn := newTurn("")
	if err := writeTurn(p, turn); err != nil {
		t.Fatal(err)
	}
	got, reason := loadTurn(p)
	if reason != "" || got == nil || num(got["schema_version"]) != 1 {
		t.Fatalf("loadTurn = (%v, %q)", got, reason)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"turn_nonce", "opened_at", "observations", "read_count", "full_read_count", "targeted_read_count", "unknown_read_count", "stat_operations", "bytes_scanned", "lines_scanned", "task_association_known", "task_id", "project_key", "codex_boundary_index", "unknown_reason", "latency"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing journal field %q", key)
		}
	}
}

func TestReadEdgeCases(t *testing.T) {
	turn := newTurn("")
	setReason(turn, "")
	if turn["unknown_reason"] != nil {
		t.Fatalf("empty reason changed null: %#v", turn["unknown_reason"])
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := expand("~"); got != filepath.Clean(home) {
		t.Fatalf("expand(~) = %q, want %q", got, home)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	usernameHome := home
	if runtime.GOOS == "windows" {
		usernameHome = filepath.Join(filepath.Dir(home), current.Username)
	}
	if got, want := expand("~"+current.Username+string(filepath.Separator)+"state"), filepath.Join(usernameHome, "state"); got != want {
		t.Fatalf("expand(~username) = %q, want %q", got, want)
	}
	p := filepath.Join(t.TempDir(), "oversized.json")
	turn["padding"] = strings.Repeat("x", maxJournal)
	err = writeTurn(p, turn)
	var sizeErr journalLimitError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("oversized write error = %T %v", err, err)
	}
	binary := filepath.Join(t.TempDir(), "binary.txt")
	if err := os.WriteFile(binary, []byte("one\x00two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	obs := scanRead(newTurn(""), binary, info, "FULL", map[string]any{})
	if obs["line_kind"] != "UNKNOWN" || obs["binary_suspected"] != true || obs["reason"] != "BINARY_SUSPECTED" {
		t.Fatalf("binary observation = %#v", obs)
	}
}

func TestProtectedProfileParseFailure(t *testing.T) {
	for _, input := range []string{
		"schema_version = 1\n[paths]\nprotected = [\"private/**\"\n",
		"schema_version = 1\n[paths]\nprotected = 7\n",
		"schema_version = 2\n[paths]\nprotected = [\"private/**\"]\n",
		"schema_version = 1\n[paths]\nprotected = [\"bad\\x2e\"]\n",
	} {
		if paths, ok := parseProtectedProfile([]byte(input)); ok || len(paths) != 0 {
			t.Fatalf("invalid profile accepted: paths=%v", paths)
		}
	}
}

func TestReadLockContentionWritesMarker(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	key := sessionKey("contention-session")
	lock := filepath.Join(state, "context_io", "locks", key+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tryLock(f); err != nil {
		t.Fatal(err)
	}
	defer unlock(f)
	if err := Read(map[string]any{"session_id": "contention-session", "hook_event_name": "PreToolUse", "tool_name": "Read"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(state, "context_io", "contention", key)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("contention marker not written: %v", err)
	}
}
