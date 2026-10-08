package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPostQuotaPythonOracleValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	now := time.Now()
	folder := filepath.Join(home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "rollout-snapshot-thread-post.jsonl")
	event := map[string]any{"timestamp": "2026-10-05T11:00:00Z", "payload": map[string]any{"rate_limits": map[string]any{
		"primary":   map[string]any{"window_minutes": 300, "used_percent": 15, "resets_at": 1791201600},
		"secondary": map[string]any{"window_minutes": 10080, "used_percent": 80, "resets_at": 1791201600},
	}}}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"snapshot_at": "2026-10-05T11:00:00Z", "5h_used_pct": float64(15), "5h_resets_at": "2026-10-05T12:00:00Z",
		"7d_used_pct": float64(80), "7d_resets_at": "2026-10-05T12:00:00Z"}
	if got := postReadQuota("thread-post"); !reflect.DeepEqual(got, want) {
		t.Fatalf("quota=%#v want %#v", got, want)
	}
	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })
	wantMessage := "⚓ Purser: Codex quota left: 5h 85% (resets 12:00) · 7d 20% (resets Oct 5)"
	if got := postQuotaSystemMessage(want); got != wantMessage {
		t.Fatalf("quota message=%q want %q", got, wantMessage)
	}
}
