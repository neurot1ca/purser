package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteUndeliveredMarkerUsesCallKeyForSyncAndStart(t *testing.T) {
	root := t.TempDir()
	for _, tool := range []string{"mcp__plugin_purser_codex-main__codex", "mcp__plugin_purser_codex-main__codex_start"} {
		event := map[string]any{
			"hook_event_name": "WriteUndeliveredMarker", "session_id": "p67-session",
			"tool_use_id": "p67-call-" + tool, "tool_name": tool, "reason": "adapter failed",
		}
		sum := sha256.Sum256([]byte(event["session_id"].(string) + "\x00" + event["tool_use_id"].(string)))
		callKey := hex.EncodeToString(sum[:])
		if err := WriteUndeliveredMarker(root, event); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "pending", "undelivered-"+callKey+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var marker map[string]any
		if err = json.Unmarshal(data, &marker); err != nil {
			t.Fatal(err)
		}
		if marker["schema"] != "purser.undelivered.v1" || marker["call_key"] != callKey ||
			marker["tool_name"] != tool || marker["reason"] != "adapter failed" {
			t.Fatalf("marker = %#v", marker)
		}
		recordedAt, ok := marker["recorded_at"].(string)
		if !ok {
			t.Fatalf("recorded_at = %#v", marker["recorded_at"])
		}
		if _, err = time.Parse(time.RFC3339Nano, recordedAt); err != nil {
			t.Fatalf("recorded_at = %q: %v", recordedAt, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "pending"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("pending entries = %v, want only both marker files", entries)
	}
}
