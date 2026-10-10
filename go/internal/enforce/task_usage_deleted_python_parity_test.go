package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestDeletedPythonTranscriptUsageParity(t *testing.T) {
	const session = "parity-session"
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	t.Run("test_contradictory_duplicate_is_terminal_across_later_stop_and_rebuild", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		writeParityTranscript(t, transcript,
			parityAssistant(session, "msg-1", "req-1", "call-1", 100, 50),
			parityAssistant(session, "msg-1", "req-1", "call-1", 150, 50),
		)
		started := startActiveTask(root, session, "task-conflict", "0123456789abcdef", "call-1", transcript, projects, now)
		if started["status"] != "STARTED" {
			t.Fatalf("start = %#v", started)
		}
		got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		if got["status"] != "FAILED" || got["reason"] != "CONTRADICTORY_USAGE_RECORD" {
			t.Fatalf("stop = %#v", got)
		}
		active, err := readActiveTask(root, session)
		if err != nil || active["invalidated_reason"] != "CONTRADICTORY_USAGE_RECORD" || active["completed"] != true {
			t.Fatalf("terminal active record = %#v, %v", active, err)
		}
	})

	t.Run("test_partial_trailing_record_at_stop_fails_closed", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		writeParityTranscript(t, transcript, parityAssistant(session, "msg-1", "req-1", "call-1", 100, 20))
		if got := startActiveTask(root, session, "task-tail", "0123456789abcdef", "call-1", transcript, projects, now); got["status"] != "STARTED" {
			t.Fatalf("start = %#v", got)
		}
		file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.WriteString(`{"type":"assistant"`)
		_ = file.Close()
		got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		if got["status"] != "FAILED" || got["reason"] != "TRANSCRIPT_UNRESOLVED_TAIL" {
			t.Fatalf("stop = %#v", got)
		}
	})

	t.Run("test_counterexample_transcript_substitution_sequence", func(t *testing.T) {
		root, projects := parityRoots(t)
		dirA, dirB := filepath.Join(projects, "a"), filepath.Join(projects, "b")
		if err := os.MkdirAll(dirB, 0700); err != nil {
			t.Fatal(err)
		}
		pathA := filepath.Join(dirA, session+".jsonl")
		pathB := filepath.Join(dirB, session+".jsonl")
		writeParityTranscript(t, pathA, parityAssistant(session, "msg-a", "req-a", "call-1", 100, 20))
		writeParityTranscript(t, pathB, parityAssistant(session, "msg-b", "req-b", "call-1", 500, 100))
		if got := startActiveTask(root, session, "task-sub", "0123456789abcdef", "call-1", pathA, projects, now); got["status"] != "STARTED" {
			t.Fatalf("start = %#v", got)
		}
		got := finalizeStopTask(root, session, pathB, projects, time.Second, now)
		if got["status"] != "FAILED" || got["reason"] != "TRANSCRIPT_IDENTITY_MISMATCH" {
			t.Fatalf("stop = %#v", got)
		}
	})

	t.Run("test_same_path_replacement_or_rotation_detected", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		writeParityTranscript(t, transcript, parityAssistant(session, "msg-1", "req-1", "call-1", 100, 20))
		if got := startActiveTask(root, session, "task-rotation", "0123456789abcdef", "call-1", transcript, projects, now); got["status"] != "STARTED" {
			t.Fatalf("start = %#v", got)
		}
		replacement := filepath.Join(projects, session+".new")
		if err := os.WriteFile(replacement, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(transcript); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, transcript); err != nil {
			t.Fatal(err)
		}
		got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		if got["status"] != "FAILED" || (got["reason"] != "TRANSCRIPT_IDENTITY_MISMATCH" && got["reason"] != "TRANSCRIPT_ROTATED_OR_TRUNCATED") {
			t.Fatalf("stop = %#v", got)
		}
	})

	t.Run("test_ctime_identity_check_is_windows_only", func(t *testing.T) {
		_, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		writeParityTranscript(t, transcript, parityAssistant(session, "msg-1", "req-1", "call-1", 1, 1))
		identity, reason := snapshotTranscript(transcript)
		if reason != "" {
			t.Fatal(reason)
		}
		active := map[string]any{"canonical_transcript_path": identity.path}
		_, _, ctime := taskUsageFileIdentity(identity.info)
		active["transcript_ctime_ns"] = ctime
		if runtime.GOOS == "windows" && ctime != nil {
			active["transcript_ctime_ns"] = int64(0)
			if got := checkStopFinalizeRecordIdentity(identity, active); got != "TRANSCRIPT_IDENTITY_MISMATCH" {
				t.Fatalf("identity reason = %q", got)
			}
		} else if got := checkStopFinalizeRecordIdentity(identity, active); got != "" {
			t.Fatalf("non-Windows ctime reason = %q", got)
		}
	})

	t.Run("test_transcript_larger_than_50mb_succeeds_with_small_segment", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		padding := append(append([]byte(`{"type":"padding","d":"`), bytes.Repeat([]byte{'x'}, 999_000)...), []byte(`"}`+"\n")...)
		data := bytes.Repeat(padding, 51)
		anchor, _ := json.Marshal(parityAssistant(session, "msg-1", "req-1", "call-1", 400, 100))
		data = append(data, append(anchor, '\n')...)
		if int64(len(data)) <= maxTaskUsageBytes {
			t.Fatalf("fixture size = %d, want > %d", len(data), maxTaskUsageBytes)
		}
		if err := os.WriteFile(transcript, data, 0600); err != nil {
			t.Fatal(err)
		}
		started := startActiveTask(root, session, "task-large", "0123456789abcdef", "call-1", transcript, projects, now)
		if started["status"] != "STARTED" || started["active_task"].(map[string]any)["anchor_deferred"] != false {
			t.Fatalf("start = %#v", started)
		}
		appendParityTranscript(t, transcript, parityAssistant(session, "msg-2", "req-2", "", 100, 25))
		got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		usage := mapValue(got["usage"])
		if got["status"] != "COMPLETED" || usage["claude_task_input_tokens"] != int64(500) || usage["claude_task_output_tokens"] != int64(125) {
			t.Fatalf("stop = %#v", got)
		}
	})

	t.Run("test_bytes_consumed_from_cursor_enforces_limit", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "transcript-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := file.Truncate(maxTaskUsageBytes + 1); err != nil {
			t.Fatal(err)
		}
		if _, reason := readTaskUsageRecords(file, 1, false); reason != "BOUNDS_EXCEEDED" {
			t.Fatalf("reason = %q", reason)
		}
	})

	t.Run("test_line_length_and_record_count_bounds_enforced", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "transcript-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(append([]byte{'{'}, bytes.Repeat([]byte{' '}, maxTaskUsageLine+1)...), []byte("}\n")...)); err != nil {
			t.Fatal(err)
		}
		if _, reason := readTaskUsageRecords(file, 0, false); reason != "BOUNDS_EXCEEDED" {
			t.Fatalf("line reason = %q", reason)
		}
		_ = file.Close()

		file, err = os.CreateTemp(t.TempDir(), "transcript-*.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		line := []byte("{}\n")
		for i := 0; i <= maxTaskUsageRows; i++ {
			if _, err := file.Write(line); err != nil {
				t.Fatal(err)
			}
		}
		if _, reason := readTaskUsageRecords(file, 0, false); reason != "BOUNDS_EXCEEDED" {
			t.Fatalf("record reason = %q", reason)
		}
		if _, err := file.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		if _, _, _, reason := scanTaskUsageAnchor(file, "missing-tool"); reason != "BOUNDS_EXCEEDED" {
			t.Fatalf("anchor record reason = %q", reason)
		}
		_ = file.Close()
	})

	t.Run("test_duplicate_stop_is_idempotent", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		writeParityTranscript(t, transcript, parityAssistant(session, "msg-1", "req-1", "call-1", 100, 20))
		if got := startActiveTask(root, session, "task-idem", "0123456789abcdef", "call-1", transcript, projects, now); got["status"] != "STARTED" {
			t.Fatalf("start = %#v", got)
		}
		first := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		second := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		if first["status"] != "COMPLETED" || second != nil {
			t.Fatalf("first=%#v second=%#v", first, second)
		}
		eventPath := filepath.Join(root, "telemetry", "events", "by_task", "task-idem.jsonl")
		data, err := os.ReadFile(eventPath)
		if err != nil {
			t.Fatal(err)
		}
		if lines := bytes.Count(data, []byte("\n")); lines != 1 {
			t.Fatalf("usage event lines = %d, want 1", lines)
		}
	})

	t.Run("test_repeated_same_task_codex_reply_retains_baseline", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		r1 := parityAssistant(session, "msg-1", "req-1", "call-1", 200, 50)
		writeParityTranscript(t, transcript, r1)
		first := startActiveTask(root, session, "task-reply", "0123456789abcdef", "call-1", transcript, projects, now)
		second := startActiveTask(root, session, "task-reply", "0123456789abcdef", "call-2", transcript, projects, now)
		if first["status"] != "STARTED" || second["status"] != "RETAINED" || second["active_task"].(map[string]any)["tool_use_id"] != "call-1" {
			t.Fatalf("starts = %#v / %#v", first, second)
		}
		appendParityTranscript(t, transcript, parityAssistant(session, "msg-2", "req-2", "", 300, 70))
		got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		usage := mapValue(got["usage"])
		if got["status"] != "COMPLETED" || usage["claude_task_input_tokens"] != int64(500) || usage["claude_task_request_count"] != 2 {
			t.Fatalf("stop = %#v", got)
		}
	})

	t.Run("test_counterexample_overlap_sequence_stop_and_rebuild_cannot_overwrite", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		writeParityTranscript(t, transcript, parityAssistant(session, "msg-1", "req-1", "call-1", 200, 50))
		if got := startActiveTask(root, session, "task-1", "0123456789abcdef", "call-1", transcript, projects, now); got["status"] != "STARTED" {
			t.Fatalf("first start = %#v", got)
		}
		if got := startActiveTask(root, session, "task-2", "0123456789abcdef", "call-2", transcript, projects, now); got["reason"] != "OVERLAPPING_TASK_BOUNDARY" {
			t.Fatalf("overlap = %#v", got)
		}
		got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		if got["reason"] != "OVERLAPPING_TASK_BOUNDARY" {
			t.Fatalf("stop = %#v", got)
		}
		active, err := readActiveTask(root, session)
		if err != nil || active["invalidated_reason"] != "OVERLAPPING_TASK_BOUNDARY" {
			t.Fatalf("active = %#v, %v", active, err)
		}
	})

	t.Run("test_session_recovery_after_overlap", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		r1 := parityAssistant(session, "msg-1", "req-1", "call-1", 100, 20)
		writeParityTranscript(t, transcript, r1)
		_ = startActiveTask(root, session, "task-1", "0123456789abcdef", "call-1", transcript, projects, now)
		_ = startActiveTask(root, session, "task-2", "0123456789abcdef", "call-2", transcript, projects, now)
		_ = finalizeStopTask(root, session, transcript, projects, time.Second, now)
		appendParityTranscript(t, transcript, parityAssistant(session, "msg-3", "req-3", "call-3", 300, 75))
		if got := startActiveTask(root, session, "task-3", "0123456789abcdef", "call-3", transcript, projects, now); got["status"] != "STARTED" {
			t.Fatalf("recovery start = %#v", got)
		}
	})

	t.Run("test_session_recovery_after_contention", func(t *testing.T) {
		root, _ := parityRoots(t)
		if got := markTaskUsageContention(root, session, "task-old"); got != "CREATED" {
			t.Fatal(got)
		}
		got := startActiveTask(root, session, "task-new", "0123456789abcdef", "call-new", "", "", now)
		if got["status"] != "STARTED" {
			t.Fatalf("recovery start = %#v", got)
		}
		if present, _ := readTaskUsageContention(root, session); present {
			t.Fatal("contention marker remains after recovery")
		}
	})

	t.Run("test_stale_invalid_active_record_is_recoverable", func(t *testing.T) {
		root, _ := parityRoots(t)
		activePath := activeTaskPath(root, session)
		old := now.Add(-25 * time.Hour).Format(time.RFC3339Nano)
		if err := writeActiveTaskJSON(activePath, map[string]any{"session_id": session, "task_id": "task-old", "tool_use_id": "call-old", "created_at": old, "completed": true, "invalidated": true, "invalidated_reason": "OVERLAPPING_TASK_BOUNDARY", "affected_task_ids": []any{"task-old"}}); err != nil {
			t.Fatal(err)
		}
		got := startActiveTask(root, session, "task-new", "0123456789abcdef", "call-new", "", "", now)
		if got["status"] != "STARTED" || got["active_task"].(map[string]any)["task_id"] != "task-new" {
			t.Fatalf("start = %#v", got)
		}
	})

	t.Run("test_deterministic_different_task_race", func(t *testing.T) {
		root, _ := parityRoots(t)
		results := make(chan map[string]any, 2)
		var wg sync.WaitGroup
		for _, task := range []string{"task-r1", "task-r2"} {
			wg.Add(1)
			go func(task string) {
				defer wg.Done()
				results <- startActiveTask(root, session, task, "0123456789abcdef", "call-"+task, "", "", now)
			}(task)
		}
		wg.Wait()
		close(results)
		started, denied := 0, 0
		for result := range results {
			if result["status"] == "STARTED" {
				started++
			}
			if result["status"] == "DENIED" {
				denied++
			}
		}
		if started != 1 || denied != 1 {
			t.Fatalf("race statuses started=%d denied=%d", started, denied)
		}
	})

	t.Run("test_deterministic_same_task_race", func(t *testing.T) {
		root, _ := parityRoots(t)
		results := make(chan map[string]any, 2)
		var wg sync.WaitGroup
		for _, tool := range []string{"call-a", "call-b"} {
			wg.Add(1)
			go func(tool string) {
				defer wg.Done()
				results <- startActiveTask(root, session, "task-same", "0123456789abcdef", tool, "", "", now)
			}(tool)
		}
		wg.Wait()
		close(results)
		statuses := map[string]bool{}
		for result := range results {
			statuses[result["status"].(string)] = true
		}
		if !statuses["STARTED"] || !statuses["RETAINED"] || len(statuses) != 2 {
			t.Fatalf("race statuses = %#v", statuses)
		}
	})

	t.Run("test_atomic_contention_marker_multiprocess_race", func(t *testing.T) {
		root, _ := parityRoots(t)
		const workers = 8
		outcomes := make(chan string, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				outcomes <- markTaskUsageContention(root, session, "task-race-"+string(rune('a'+i)))
			}(i)
		}
		wg.Wait()
		close(outcomes)
		created, exists := 0, 0
		for outcome := range outcomes {
			if outcome == "CREATED" {
				created++
			}
			if outcome == "EXISTS" {
				exists++
			}
		}
		if created != 1 || exists != workers-1 {
			t.Fatalf("marker outcomes created=%d exists=%d", created, exists)
		}
		present, id := readTaskUsageContention(root, session)
		if !present || id == "" {
			t.Fatalf("marker present=%t id=%q", present, id)
		}
	})

	t.Run("test_later_contender_cannot_overwrite_marker", func(t *testing.T) {
		root, _ := parityRoots(t)
		if got := markTaskUsageContention(root, session, "task-first"); got != "CREATED" {
			t.Fatal(got)
		}
		if got := markTaskUsageContention(root, session, "task-second"); got != "EXISTS" {
			t.Fatal(got)
		}
		present, id := readTaskUsageContention(root, session)
		if !present || id != "task-first" {
			t.Fatalf("marker present=%t id=%q", present, id)
		}
	})

	t.Run("test_empty_marker_invalidates_active_boundary_at_stop", func(t *testing.T) {
		root, projects := parityRoots(t)
		transcript := filepath.Join(projects, session+".jsonl")
		writeParityTranscript(t, transcript, parityAssistant(session, "msg-1", "req-1", "call-1", 100, 20))
		if got := startActiveTask(root, session, "task-empty", "0123456789abcdef", "call-1", transcript, projects, now); got["status"] != "STARTED" {
			t.Fatalf("start = %#v", got)
		}
		if got := markTaskUsageContention(root, session, ""); got != "CREATED" {
			t.Fatal(got)
		}
		present, id := readTaskUsageContention(root, session)
		if !present || id != "" {
			t.Fatalf("marker present=%t id=%q", present, id)
		}
		got := finalizeStopTask(root, session, transcript, projects, time.Second, now)
		if got["status"] != "FAILED" || got["reason"] != "ACTIVE_TASK_STATE_CONTENTION" {
			t.Fatalf("stop = %#v", got)
		}
		active, err := readActiveTask(root, session)
		if err != nil || active["invalidated_reason"] != "ACTIVE_TASK_STATE_CONTENTION" {
			t.Fatalf("active = %#v, %v", active, err)
		}
	})
}

func parityRoots(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0700); err != nil {
		t.Fatal(err)
	}
	return root, projects
}

func parityAssistant(session, message, request, tool string, input, output int64) map[string]any {
	content := []any{}
	if tool != "" {
		content = append(content, map[string]any{"type": "tool_use", "id": tool})
	}
	return map[string]any{
		"type": "assistant", "sessionId": session, "requestId": request,
		"message": map[string]any{
			"id": message, "content": content,
			"usage": map[string]any{"input_tokens": input, "output_tokens": output, "cache_creation_input_tokens": int64(0), "cache_read_input_tokens": int64(0)},
		},
	}
}

func writeParityTranscript(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func appendParityTranscript(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}
