package enforce

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTaskUsageP59Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "task_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name, Op, File, Session, Content, Tool, Expected string
			Outside                                          bool
			Offset                                           int64
			Anchor                                           *int64
			Cursor                                           int64
			Deferred                                         bool
			Records                                          int
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Oracle == "" {
		t.Fatal("missing Python oracle")
	}
	for _, c := range f.Cases {
		if len(c.Name) < 3 || c.Name[:3] != "p59" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			projects := filepath.Join(root, "projects")
			if err := os.Mkdir(projects, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(projects, c.File)
			if c.Op == "path" {
				if c.File == "" {
					path = ""
				}
				if c.File != "" && c.File != "missing.jsonl" && !c.Outside {
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if c.Outside {
					path = filepath.Join(root, c.File)
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				_, got := validateTranscriptPath(path, c.Session, projects)
				if got != c.Expected {
					t.Fatalf("reason %q want %q", got, c.Expected)
				}
				return
			}
			path = filepath.Join(projects, "session-1.jsonl")
			if err := os.WriteFile(path, []byte(c.Content), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var records []map[string]any
			var reason string
			switch c.Op {
			case "read", "read-allow", "read-offset":
				offset := int64(0)
				if c.Op == "read-offset" {
					offset = c.Offset
				}
				records, reason = readTaskUsageRecords(file, offset, c.Op == "read-allow")
			case "anchor":
				anchor, cursor, deferred, why := scanTaskUsageAnchor(file, c.Tool)
				reason = why
				if why == "" && (anchor != nil) != (c.Anchor != nil) {
					t.Fatalf("anchor presence %v", anchor)
				}
				if why == "" && anchor != nil && *anchor != *c.Anchor {
					t.Fatalf("anchor %d want %d", *anchor, *c.Anchor)
				}
				if why == "" && (cursor != c.Cursor || deferred != c.Deferred) {
					t.Fatalf("cursor/deferred %d/%t want %d/%t", cursor, deferred, c.Cursor, c.Deferred)
				}
			}
			if reason != c.Expected {
				t.Fatalf("reason %q want %q", reason, c.Expected)
			}
			if reason == "" && c.Op != "anchor" && len(records) != c.Records {
				t.Fatalf("records %d want %d", len(records), c.Records)
			}
		})
	}
}

func TestTaskUsageP60Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "task_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name, Op, Session, Task, Other, Reason, Expected string
			MarkerID                                         string `json:"marker_id"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		if len(c.Name) < 3 || c.Name[:3] != "p60" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			switch c.Op {
			case "lock":
				file, acquired := acquireTaskUsageLock(root, c.Session, 20*time.Millisecond)
				if acquired != (c.Expected == "true") {
					t.Fatalf("acquired %t want %s", acquired, c.Expected)
				}
				releaseTaskUsageLock(file)
			case "lock-timeout":
				held, acquired := acquireTaskUsageLock(root, c.Session, 20*time.Millisecond)
				if !acquired {
					t.Fatal("first lock not acquired")
				}
				defer releaseTaskUsageLock(held)
				contender, acquired := acquireTaskUsageLock(root, c.Session, 20*time.Millisecond)
				if acquired != (c.Expected == "true") {
					t.Fatalf("contender acquired %t want %s", acquired, c.Expected)
				}
				releaseTaskUsageLock(contender)
			case "marker-compete":
				first := markTaskUsageContention(root, c.Session, c.Task)
				second := markTaskUsageContention(root, c.Session, c.Other)
				present, taskID := readTaskUsageContention(root, c.Session)
				if first+","+second != c.Expected || !present || taskID != c.MarkerID {
					t.Fatalf("marker %s,%s present=%t id=%q; want %s and %q", first, second, present, taskID, c.Expected, c.MarkerID)
				}
			case "marker-existing":
				if got := markTaskUsageContention(root, c.Session, c.Task); got != "CREATED" {
					t.Fatal(got)
				}
				present, taskID := readTaskUsageContention(root, c.Session)
				if !present || taskID != c.MarkerID || c.Expected != "true" {
					t.Fatalf("present=%t id=%q", present, taskID)
				}
			case "marker-clear-failure":
				if err := os.MkdirAll(filepath.Join(root, "active_tasks"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(taskUsageContentionPath(root, c.Session), 0700); err != nil {
					t.Fatal(err)
				}
				if present, taskID := readTaskUsageContention(root, c.Session); !present || taskID != "" {
					t.Fatalf("unreadable marker present=%t id=%q", present, taskID)
				}
				if got := removeTaskUsageContention(root, c.Session); got != (c.Expected == "true") {
					t.Fatalf("removed %t want %s", got, c.Expected)
				}
			case "terminal-preserved":
				usage := map[string]any{"claude_task_usage_unknown_reason": c.Reason, "claude_task_usage_known": false}
				foldClaudeTaskUsage(usage, map[string]any{"claude_task_usage_known": true, "claude_task_usage_complete": true, "claude_task_input_tokens": float64(5)})
				if usage["claude_task_usage_unknown_reason"] != c.Expected || usage["claude_task_usage_known"] != false {
					t.Fatalf("terminal usage was overwritten: %#v", usage)
				}
			default:
				t.Fatalf("unknown p60 op %q", c.Op)
			}
		})
	}
}

func TestTaskUsageP59IdentityAndOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, reason := snapshotTranscript(path)
	if reason != "" {
		t.Fatal(reason)
	}
	if got := checkTranscriptIdentity(path, id, nil, nil); got != "" {
		t.Fatal(got)
	}
	wrong := *id
	wrong.path = "elsewhere"
	if got := checkTranscriptIdentity(path, &wrong, nil, nil); got != "TRANSCRIPT_IDENTITY_MISMATCH" {
		t.Fatal(got)
	}
	anchor := int64(4)
	if got := checkTranscriptIdentity(path, id, &anchor, nil); got != "TRANSCRIPT_ROTATED_OR_TRUNCATED" {
		t.Fatal(got)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := checkTranscriptIdentity(path, id, nil, nil); got != "TRANSCRIPT_ROTATED_OR_TRUNCATED" {
		t.Fatal(got)
	}
	if got := checkTranscriptIdentity(filepath.Join(filepath.Dir(path), "gone.jsonl"), id, nil, nil); got != "TRANSCRIPT_UNAVAILABLE" {
		t.Fatal(got)
	}
}

type countedSeeker struct{ pos, size, read int64 }

func (r *countedSeeker) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.size-r.pos {
		n = r.size - r.pos
	}
	clear(p[:n])
	r.pos += n
	r.read += n
	return int(n), nil
}
func (r *countedSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = offset
	case io.SeekCurrent:
		r.pos += offset
	case io.SeekEnd:
		r.pos = r.size + offset
	}
	return r.pos, nil
}

func TestTaskUsageP59AnchorSeeksLargeFileTail(t *testing.T) {
	const size = int64(100_000_000)
	reader := &countedSeeker{size: size}
	_, _, _, reason := scanTaskUsageTail(reader, size, "absent")
	if reason != "" {
		t.Fatal(reason)
	}
	if reader.read >= size || reader.read > maxTaskUsageBytes+64*1024 {
		t.Fatalf("read %d bytes of %d", reader.read, size)
	}
}

func TestTaskUsageP59ReadBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(maxTaskUsageBytes + 1); err != nil {
		t.Fatal(err)
	}
	if _, reason := readTaskUsageRecords(file, 0, false); reason != "BOUNDS_EXCEEDED" {
		t.Fatalf("size bound: %q", reason)
	}
	file.Truncate(0)
	file.Seek(0, 0)
	file.Write(bytes.Repeat([]byte("x"), maxTaskUsageLine+1))
	file.Write([]byte("\n"))
	if _, reason := readTaskUsageRecords(file, 0, false); reason != "BOUNDS_EXCEEDED" {
		t.Fatalf("line bound: %q", reason)
	}
	file.Close()
}
