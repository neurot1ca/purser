package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStopFinalizeP130Writes(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	t.Run("missing identifiers unlink", func(t *testing.T) {
		root := t.TempDir()
		path := activeTaskPath(root, "p130-missing")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"task_id":"task"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if got := finalizeStopTask(root, "p130-missing", "", "", time.Second, now); got != nil {
			t.Fatalf("got %v", got)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("active file remains: %v", err)
		}
	})
	t.Run("stale task is finalized and observed", func(t *testing.T) {
		root := t.TempDir()
		path := activeTaskPath(root, "p130-stale")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		active := map[string]any{"task_id": "task-p130", "tool_use_id": "tool-p130", "project_key": "0123456789abcdef", "created_at": "2026-10-03T11:59:59Z", "completed": false, "invalidated": false}
		data, _ := json.Marshal(active)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		got := finalizeStopTask(root, "p130-stale", "", "", time.Second, now)
		if got["reason"] != "STALE_ACTIVE_TASK" {
			t.Fatalf("got %v", got)
		}
		updated, err := readActiveTask(root, "p130-stale")
		if err != nil || updated["invalidated"] != true || updated["completed"] != true {
			t.Fatalf("record %v, err %v", updated, err)
		}
		if _, err := os.Stat(filepath.Join(root, "telemetry", "events", "by_task", "task-p130.jsonl")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestClaudeProjectsRootUsesConfiguredAndDefaultConfigDirs(t *testing.T) {
	home, config := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	if got, want := claudeProjectsRoot(), filepath.Join(config, "projects"); got != want {
		t.Fatalf("configured Claude projects root = %q, want %q", got, want)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got, want := claudeProjectsRoot(), filepath.Join(home, ".claude", "projects"); got != want {
		t.Fatalf("default Claude projects root = %q, want %q", got, want)
	}
}

func TestStopFinalizeKeepsSessionLockAcrossDecisionAndWrites(t *testing.T) {
	root := t.TempDir()
	session := "p131-lock-race"
	path := activeTaskPath(root, session)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	active := map[string]any{"task_id": "task-original", "tool_use_id": "tool-original", "project_key": "0123456789abcdef", "completed": false, "invalidated": false}
	data, _ := json.Marshal(active)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	lock, acquired := acquireTaskUsageLock(root, session, time.Second)
	if !acquired {
		t.Fatal("failed to acquire test lock")
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = finalizeStopTask(root, session, "", "", time.Second, time.Now())
	}()
	time.Sleep(30 * time.Millisecond)
	latest := map[string]any{"task_id": "task-latest", "tool_use_id": "tool-latest", "project_key": "0123456789abcdef", "completed": false, "invalidated": true, "invalidated_reason": "NEWER_FINALIZATION"}
	data, _ = json.Marshal(latest)
	if err := os.WriteFile(path, data, 0600); err != nil {
		releaseTaskUsageLock(lock)
		t.Fatal(err)
	}
	releaseTaskUsageLock(lock)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("finalizer did not finish")
	}
	got, err := readActiveTask(root, session)
	if err != nil || got["task_id"] != "task-latest" || got["invalidated_reason"] != "NEWER_FINALIZATION" {
		t.Fatalf("finalizer overwrote the record read before lock: %v, %v", got, err)
	}
}

func TestStopFinalizeP130PythonFiles(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "stop_finalize_writes.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string `json:"name"`
			Expected struct {
				Result     any              `json:"result"`
				Active     map[string]any   `json:"active"`
				Events     []map[string]any `json:"events"`
				Projection map[string]any   `json:"projection"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "claude_transcript_usage.finalize_stop_task" {
		t.Fatalf("oracle %q", fixture.Oracle)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			session, taskID, toolID := "p130-"+c.Name, "task-"+c.Name, "tool-"+c.Name
			projects := filepath.Join(root, "projects_allowed")
			if err := os.MkdirAll(projects, 0700); err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(projects, session+".jsonl")
			row := map[string]any{"type": "assistant", "requestId": "req-anchor", "message": map[string]any{"id": "anchor", "content": []any{map[string]any{"type": "tool_use", "id": toolID}}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2}}}
			encoded, _ := json.Marshal(row)
			if err := os.WriteFile(transcript, append(encoded, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			identity, reason := snapshotTranscript(transcript)
			if reason != "" {
				t.Fatal(reason)
			}
			dev, ino, ctime := taskUsageFileIdentity(identity.info)
			active := map[string]any{"task_id": taskID, "tool_use_id": toolID, "project_key": "0123456789abcdef", "canonical_transcript_path": identity.path, "transcript_dev": dev, "transcript_ino": ino, "transcript_ctime_ns": ctime, "initial_size": identity.size, "anchor_byte_offset": int64(0), "anchor_search_start_offset": int64(0), "anchor_deferred": false, "created_at": "2026-10-05T11:00:00Z", "completed": false, "invalidated": false, "affected_task_ids": []any{taskID}}
			if c.Name == "missing-ids" {
				delete(active, "task_id")
			}
			if c.Name == "stale" {
				active["created_at"] = "2026-10-03T11:59:59Z"
			}
			if c.Name == "identity-failure" {
				active["canonical_transcript_path"] = filepath.Join(projects, "different.jsonl")
			}
			activePath := activeTaskPath(root, session)
			if err := os.MkdirAll(filepath.Dir(activePath), 0700); err != nil {
				t.Fatal(err)
			}
			activeJSON, _ := json.Marshal(active)
			if err := os.WriteFile(activePath, activeJSON, 0600); err != nil {
				t.Fatal(err)
			}
			if c.Name == "contention" {
				markTaskUsageContention(root, session, "task-competing")
			}
			if c.Name == "persist-failure" {
				events := filepath.Join(root, "telemetry", "events")
				if err := os.MkdirAll(filepath.Dir(events), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(events, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
			if c.Name == "repeat" {
				got = map[string]any{"first": got, "second": finalizeStopTask(root, session, transcript, projects, time.Second, now)}
			}
			actualActive, readErr := readActiveTask(root, session)
			if os.IsNotExist(readErr) {
				actualActive = nil
			} else if readErr != nil {
				t.Fatal(readErr)
			}
			if actualActive != nil {
				actualActive["canonical_transcript_path"] = "<transcript>"
				delete(actualActive, "transcript_dev")
				delete(actualActive, "transcript_ino")
				delete(actualActive, "transcript_ctime_ns")
			}
			eventPath := filepath.Join(root, "telemetry", "events", "by_task", taskID+".jsonl")
			events := []map[string]any{}
			if eventData, err := os.ReadFile(eventPath); err == nil {
				for _, line := range bytes.Split(bytes.TrimSpace(eventData), []byte{'\n'}) {
					var event map[string]any
					if json.Unmarshal(line, &event) != nil {
						t.Fatal("bad event json")
					}
					event["event_id"], event["ts_utc"] = "00000000-0000-0000-0000-000000000001", "2026-10-05T12:00:00Z"
					events = append(events, event)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			projectionPath := filepath.Join(root, "projects", "0123456789abcdef", "telemetry", "tasks", taskID+".json")
			var projection map[string]any
			if projectionData, err := os.ReadFile(projectionPath); err == nil {
				if err := json.Unmarshal(projectionData, &projection); err != nil {
					t.Fatal(err)
				}
				projection = map[string]any{"task_id": projection["task_id"], "project_key": projection["project_key"], "status": projection["status"], "usage": projection["usage"]}
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			actualJSON, _ := json.Marshal(map[string]any{"result": got, "active": actualActive, "events": events, "projection": projection})
			wantJSON, _ := json.Marshal(c.Expected)
			if !bytes.Equal(actualJSON, wantJSON) {
				var actual, expected map[string]any
				_ = json.Unmarshal(actualJSON, &actual)
				_ = json.Unmarshal(wantJSON, &expected)
				for _, field := range []string{"result", "active", "events", "projection"} {
					left, _ := json.Marshal(actual[field])
					right, _ := json.Marshal(expected[field])
					if !bytes.Equal(left, right) {
						t.Fatalf("%s differs\n got %s\nwant %s", field, left, right)
					}
				}
			}
		})
	}
}

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
