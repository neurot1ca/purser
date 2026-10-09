package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopAutoCompactSuppressesOnlyCompactionReason(t *testing.T) {
	for _, test := range []struct {
		name       string
		env        string
		logAge     time.Duration
		otherBlock bool
		wantBlock  bool
		wantQuote  bool
	}{
		{name: "fresh log with default env", logAge: 0},
		{name: "stale log", logAge: 31 * time.Minute, wantBlock: true, wantQuote: true},
		{name: "disabled by env", env: "off", logAge: 0, wantBlock: true, wantQuote: true},
		{name: "other reason still blocks", logAge: 0, otherBlock: true, wantBlock: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, home, repo := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PURSER_STATE_DIR", root)
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOME", home)
			t.Setenv("PURSER_AUTO_COMPACT_TOKENS", test.env)
			logPath := filepath.Join(root, "auto-compact.log")
			if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(logPath, []byte("decision\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if test.logAge > 0 {
				stale := time.Now().Add(-test.logAge)
				if err := os.Chtimes(logPath, stale, stale); err != nil {
					t.Fatal(err)
				}
			}
			session := "auto-compact-stop"
			state := map[string]any{"session_id": session, "notice_pending": map[string]any{
				"notice_id": "0123456789abcdef0123456789abcdef", "context": 250000,
				"percentage": 70, "handoff": "handoff.md",
			}}
			if test.otherBlock {
				state["version_pending"] = map[string]any{"version": "v-next"}
			}
			statePath := filepath.Join(root, "projects", repositoryProjectKey(repo), "telemetry", "compaction_advisor.json")
			if err := writeAdvisorState(statePath, state); err != nil {
				t.Fatal(err)
			}
			out := Stop(map[string]any{"cwd": repo, "session_id": session}, time.Now())
			var decision map[string]any
			if len(out) > 0 {
				if err := json.Unmarshal(out, &decision); err != nil {
					t.Fatal(err)
				}
			}
			gotBlock := decision != nil && decision["decision"] == "block"
			if gotBlock != test.wantBlock {
				t.Fatalf("blocked = %v, want %v (output %s)", gotBlock, test.wantBlock, out)
			}
			reason, _ := decision["reason"].(string)
			gotQuote := strings.Contains(reason, "Context passed 250K") && strings.Contains(reason, "Type `/compact` now")
			if gotQuote != test.wantQuote {
				t.Fatalf("blockquote present = %v, want %v (reason %q)", gotQuote, test.wantQuote, reason)
			}
			if test.otherBlock && !strings.Contains(reason, "v-next") {
				t.Fatalf("other Stop reason was dropped: %q", reason)
			}
		})
	}
}

func TestCompactionNoticeAdviceOmitsBlockquoteWhenAutoCompactActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PURSER_STATE_DIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	logPath := filepath.Join(home, ".claude", "purser", "auto-compact.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("decision\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := compactionNoticeAdvice(map[string]any{"context": 250000, "percentage": 70, "handoff": "handoff.md"}, []string{"threshold"})
	if strings.Contains(got, "Type `/compact` now") || strings.Contains(got, "four-line blockquote") {
		t.Fatalf("active auto-compact advice still requests a blockquote: %q", got)
	}
	if !strings.Contains(got, "Handoff: handoff.md") || !strings.Contains(got, "update its Goal, State, Decisions, Next") {
		t.Fatalf("handoff instructions were lost: %q", got)
	}
}

func TestAutoCompactUsesResolvedStateRoot(t *testing.T) {
	home, custom := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PURSER_AUTO_COMPACT_TOKENS", "")
	t.Setenv("PURSER_STATE_DIR", custom)
	writeLog := func(root string) {
		t.Helper()
		path := filepath.Join(root, "auto-compact.log")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("decision\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeLog(custom)
	if !autoCompactModActive() {
		t.Fatal("custom PURSER_STATE_DIR log was not recognized")
	}

	t.Setenv("PURSER_STATE_DIR", "")
	writeLog(filepath.Join(home, ".claude", "purser"))
	if !autoCompactModActive() {
		t.Fatal("default state root log was not recognized")
	}
}
