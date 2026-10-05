package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStopFinalizeP129Fixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "stop_usage.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name           string            `json:"name"`
			Outside        bool              `json:"outside"`
			IdentityChange bool              `json:"identity_changed"`
			Shrank         bool              `json:"shrank"`
			Transcript     []json.RawMessage `json:"transcript"`
			RawTranscript  string            `json:"raw_transcript"`
			Expected       json.RawMessage   `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			projects := filepath.Join(root, "projects")
			if err := os.MkdirAll(projects, 0700); err != nil {
				t.Fatal(err)
			}
			transcriptDir := projects
			if c.Outside {
				transcriptDir = filepath.Join(root, "outside")
				if err := os.MkdirAll(transcriptDir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			transcriptPath := filepath.Join(transcriptDir, "p129-session.jsonl")
			content := c.RawTranscript
			if content == "" {
				for _, row := range c.Transcript {
					var compact bytes.Buffer
					if err := json.Compact(&compact, row); err != nil {
						t.Fatal(err)
					}
					content += compact.String() + "\n"
				}
			}
			if err := os.WriteFile(transcriptPath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			identity, reason := snapshotTranscript(transcriptPath)
			if reason != "" {
				t.Fatal(reason)
			}
			dev, ino, ctime := taskUsageFileIdentity(identity.info)
			canonical := identity.path
			if c.IdentityChange {
				canonical = filepath.Join(projects, "different.jsonl")
			}
			initialSize := identity.size
			if c.Shrank {
				initialSize++
			}
			active := map[string]any{
				"task_id": "task-129", "tool_use_id": "tool-129", "project_key": "fixture",
				"canonical_transcript_path": canonical, "transcript_dev": dev, "transcript_ino": ino,
				"transcript_ctime_ns": ctime, "initial_size": initialSize, "anchor_byte_offset": int64(0),
				"anchor_search_start_offset": int64(0), "anchor_deferred": false,
			}
			record, err := json.Marshal(active)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(record, &active); err != nil {
				t.Fatal(err)
			}
			got := finalizeStopTaskUsage(map[string]any{"status": "READY"}, active, "p129-session", transcriptPath, projects)
			assertStopFinalizeResult(t, got, c.Expected)
		})
	}
}
