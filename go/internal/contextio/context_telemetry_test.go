package contextio

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestContextTelemetryReadAssociationAndProjection(t *testing.T) {
	state, repo, session := t.TempDir(), filepath.Join(t.TempDir(), "repo"), "context-telemetry"
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", state)
	short, long := filepath.Join(repo, "short.txt"), filepath.Join(repo, "long.txt")
	if err := os.WriteFile(short, []byte(strings.Repeat("x\n", 199)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(long, []byte(strings.Repeat("x\n", 501)), 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	UserPromptSubmit(map[string]any{"session_id": session, "cwd": repo}, started)
	for i, input := range []map[string]any{
		{"file_path": short},
		{"file_path": long},
		{"file_path": long, "offset": 3, "limit": 7},
	} {
		if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": input}, started); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if !AssociateTurn(state, session, "task-context-1", nil) {
				t.Fatal("AssociateTurn failed")
			}
		}
	}
	journal, reason := loadTurn(filepath.Join(state, "context_io", "turns", sessionKey(session)+".json"))
	if reason != "" || journal == nil {
		t.Fatalf("read journal = %#v, %q", journal, reason)
	}
	observations := journal["observations"].([]any)
	wantBytes, wantLines := []int{398, 1002, 14}, []int{199, 501, 7}
	for i, value := range observations {
		observation := value.(map[string]any)
		if number(observation["bytes"]) != wantBytes[i] || number(observation["line_value"]) != wantLines[i] || observation["line_kind"] != "EXACT" {
			t.Errorf("observation %d = %#v", i, observation)
		}
	}
	stopTurn(map[string]any{"session_id": session}, started)
	event := readOnlyContextEvent(t, state)
	if event["task_id"] != "task-context-1" || event["session_id"] != session || event["event_type"] != "CONTEXT_IO_TURN_RECORDED" || number(event["schema_version"]) != 1 || len(str(event["event_id"])) != 36 {
		t.Fatalf("event identity = %#v", event)
	}
	payload := event["payload"].(map[string]any)
	for field, want := range map[string]int{
		"read_count": 3, "full_read_count": 2, "targeted_read_count": 1,
		"pre_codex_read_count": 1, "post_codex_read_count": 2,
		"threshold_200_known_count": 3, "threshold_200_candidate_count": 1,
		"threshold_350_candidate_count": 1, "threshold_500_candidate_count": 1,
		"repeat_count": 1,
	} {
		if got := number(payload[field]); got != want {
			t.Errorf("payload[%q] = %v, want %v", field, got, want)
		}
	}
	if payload["task_association_known"] != true || payload["runtime"] != "go" {
		t.Fatalf("association/runtime = %#v / %v", payload["task_association_known"], payload["runtime"])
	}
}

func TestContextTelemetryUnknownRepeatProjection(t *testing.T) {
	turn := newTurn("")
	addObservation(turn, map[string]any{
		"bytes": nil, "line_kind": "UNKNOWN", "line_value": nil,
		"oversized": false, "binary_suspected": false, "repeat": nil,
		"thresholds": map[string]any{"200": nil, "350": nil, "500": nil},
	})
	event := contextTurnEvent(turn, "unknown-repeat", 0, 0, time.Now())
	payload := event["payload"].(map[string]any)
	if payload["repeat_known"] != false || number(payload["repeat_count"]) != 0 {
		t.Fatalf("unknown repeat projection = %#v", payload)
	}
}

func TestContextTelemetryPromptReplacesUnstoppedPriorRead(t *testing.T) {
	state, repo, session := t.TempDir(), filepath.Join(t.TempDir(), "repo"), "prior-read"
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", state)
	target := filepath.Join(repo, "notes.txt")
	if err := os.WriteFile(target, []byte("prior turn content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": target}}, started); err != nil {
		t.Fatal(err)
	}
	output := UserPromptSubmit(map[string]any{"session_id": session, "cwd": repo, "prompt": "Please plan this migration."}, started)
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("prompt response %q: %v", output, err)
	}
	hook := response["hookSpecificOutput"].(map[string]any)
	if hook["hookEventName"] != "UserPromptSubmit" || hook["additionalContext"] != "The user mentioned planning; offer /purser:plan once in this session if you have not already." {
		t.Fatalf("planning context = %#v", hook)
	}
	turn, reason := loadTurn(filepath.Join(state, "context_io", "turns", sessionKey(session)+".json"))
	if reason != "" || turn == nil || number(turn["read_count"]) != 0 || turn["unknown_reason"] != "PRIOR_TURN_MISSING_STOP" {
		t.Fatalf("new prompt turn = %#v, %q", turn, reason)
	}
	stopTurn(map[string]any{"session_id": session}, started)
	payload := readOnlyContextEvent(t, state)["payload"].(map[string]any)
	if number(payload["read_count"]) != 0 || payload["unknown_reason"] != "PRIOR_TURN_MISSING_STOP" {
		t.Fatalf("stopped prompt turn = %#v", payload)
	}
}

func TestContextTelemetryRetryUsesFrozenEnvelope(t *testing.T) {
	state, session := t.TempDir(), "context-telemetry-retry"
	t.Setenv("PURSER_STATE_DIR", state)
	started := time.Now()
	UserPromptSubmit(map[string]any{"session_id": session}, started)
	if !AssociateTurn(state, session, "retry-task", nil) {
		t.Fatal("AssociateTurn failed")
	}
	eventsRoot := filepath.Join(state, "telemetry", "events")
	if err := os.MkdirAll(filepath.Dir(eventsRoot), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsRoot, []byte("temporary blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	stopTurn(map[string]any{"session_id": session}, started)
	journal := filepath.Join(state, "context_io", "turns", sessionKey(session)+".json")
	turn, reason := loadTurn(journal)
	if reason != "" || turn == nil {
		t.Fatalf("frozen journal load = %v, %q", turn, reason)
	}
	frozen := turn["final_event"].(map[string]any)
	if err := os.Remove(eventsRoot); err != nil {
		t.Fatal(err)
	}
	stopTurn(map[string]any{"session_id": session}, started)
	event := readOnlyContextEvent(t, state)
	got, _ := json.Marshal(event)
	want, _ := json.Marshal(frozen)
	if !bytes.Equal(got, want) {
		t.Fatalf("retry event = %s, want frozen %s", got, want)
	}
}

func TestContextTelemetryUnknownAndProtectedReadClasses(t *testing.T) {
	state, repo, session := t.TempDir(), filepath.Join(t.TempDir(), "repo"), "context-read-classes"
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", state)
	UserPromptSubmit(map[string]any{"session_id": session, "cwd": repo}, time.Now())
	protected := filepath.Join(repo, "AGENTS.md")
	directory := filepath.Join(repo, "folder")
	binary := filepath.Join(repo, "binary.dat")
	oversized := filepath.Join(repo, "oversized.txt")
	for path, data := range map[string][]byte{
		protected: []byte("protected sentinel"), binary: {'a', 0, 'b'}, oversized: []byte(strings.Repeat("z", maxBytesPerRead+1)),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, protected, filepath.Join(repo, "missing.txt"), directory, binary, oversized} {
		if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": path}}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": 7}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	stopTurn(map[string]any{"session_id": session}, time.Now())
	payload := readOnlyContextEvent(t, state)["payload"].(map[string]any)
	if number(payload["read_count"]) != 7 || number(payload["unknown_read_count"]) != 5 || number(payload["binary_suspected_read_count"]) != 1 || number(payload["oversized_read_count"]) != 1 {
		t.Fatalf("read class counts = %#v", payload)
	}
	if payload["unknown_reason"] != "OUTSIDE_ROOT" || number(payload["threshold_200_known_count"]) != 0 {
		t.Fatalf("unknown state = %#v", payload)
	}
	if number(payload["lower_bound_line_read_count"]) != 1 || payload["repeat_known"] != false {
		t.Fatalf("bounded scan/repeat state = %#v", payload)
	}
}

func TestContextTelemetryTargetedReadNearEOF(t *testing.T) {
	state, repo, session := t.TempDir(), filepath.Join(t.TempDir(), "repo"), "targeted-near-eof"
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", state)
	path := filepath.Join(repo, "many-lines.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x\n", 500)), 0600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	UserPromptSubmit(map[string]any{"session_id": session, "cwd": repo}, started)
	if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": path, "offset": 490, "limit": 7}}, started); err != nil {
		t.Fatal(err)
	}
	stopTurn(map[string]any{"session_id": session}, started)
	payload := readOnlyContextEvent(t, state)["payload"].(map[string]any)
	if number(payload["observed_bytes_total"]) != 14 || number(payload["exact_line_read_count"]) != 1 || number(payload["exact_line_count_total"]) != 7 {
		t.Fatalf("targeted EOF projection = %#v", payload)
	}
}

func TestContextTelemetryProtectedProfiles(t *testing.T) {
	state, repo, session := t.TempDir(), filepath.Join(t.TempDir(), "repo"), "profile-protection"
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".orchestrator"), 0700); err != nil {
		t.Fatal(err)
	}
	projectProfile := filepath.Join(repo, ".orchestrator", "profile.toml")
	if err := os.WriteFile(projectProfile, []byte("schema_version = 1\n[paths]\nprotected = [\"private/**\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".orchestrator"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".orchestrator", "config.toml"), []byte("schema_version = 1\n[paths]\nprotected = [\"user-secret/**\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PURSER_STATE_DIR", state)
	for _, relative := range []string{"private/project.txt", "user-secret/user.txt"} {
		path := filepath.Join(repo, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("protected"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": path}}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	turn, reason := loadTurn(filepath.Join(state, "context_io", "turns", sessionKey(session)+".json"))
	if reason != "" || turn == nil {
		t.Fatalf("profile turn = %#v, %q", turn, reason)
	}
	for _, value := range turn["observations"].([]any) {
		if value.(map[string]any)["reason"] != "PROTECTED_PATH" {
			t.Fatalf("protected observation = %#v", value)
		}
	}
	if err := os.WriteFile(projectProfile, []byte("schema_version = 1\n[paths]\nprotected = [\"private/**\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(repo, "private", "allowed-after-malformed-profile.txt")
	if err := os.WriteFile(allowed, []byte("allowed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": allowed}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	turn, reason = loadTurn(filepath.Join(state, "context_io", "turns", sessionKey(session)+".json"))
	if reason != "" || turn == nil || turn["unknown_read_count"] != json.Number("2") {
		t.Fatalf("malformed project profile did not retain user protections: %#v, %q", turn, reason)
	}
	last := turn["observations"].([]any)[2].(map[string]any)
	if last["reason"] != nil {
		t.Fatalf("malformed project tier protected path: %#v", last)
	}
}

func TestContextTelemetryContentionSurvivesIntoNextTurn(t *testing.T) {
	state, session := t.TempDir(), "contention-next-turn"
	t.Setenv("PURSER_STATE_DIR", state)
	key := sessionKey(session)
	lockPath := filepath.Join(state, "context_io", "locks", key+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := Read(map[string]any{"session_id": session}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := unlock(lock); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	UserPromptSubmit(map[string]any{"session_id": session}, time.Now())
	stopTurn(map[string]any{"session_id": session}, time.Now())
	if got := readOnlyContextEvent(t, state)["payload"].(map[string]any)["unknown_reason"]; got != "STATE_CONTENTION" {
		t.Fatalf("unknown_reason = %v, want STATE_CONTENTION", got)
	}
}

func TestContextTelemetryWriteFailureFailsOpenAndMarksContention(t *testing.T) {
	state, session := t.TempDir(), "write-failure"
	t.Setenv("PURSER_STATE_DIR", state)
	journal := filepath.Join(state, "context_io", "turns", sessionKey(session)+".json")
	if err := os.MkdirAll(journal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Read(map[string]any{"session_id": session, "tool_input": map[string]any{}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	UserPromptSubmit(map[string]any{"session_id": session}, time.Now())
	stopTurn(map[string]any{"session_id": session}, time.Now())
	payload := readOnlyContextEvent(t, state)["payload"].(map[string]any)
	if payload["unknown_reason"] != "STATE_CONTENTION" || number(payload["read_count"]) != 0 {
		t.Fatalf("write failure degradation = %#v", payload)
	}
}

func TestContextTelemetryCorruptStaleAndMissingJournalReasons(t *testing.T) {
	for _, tc := range []struct{ name, contents, want string }{
		{"corrupt", "{broken", "CORRUPT_STATE"},
		{"partial", `{"schema_version":1}`, "CORRUPT_STATE"},
		{"missing", "", "MISSING_USER_PROMPT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, session := t.TempDir(), "journal-"+tc.name
			t.Setenv("PURSER_STATE_DIR", state)
			if tc.contents != "" {
				path := filepath.Join(state, "context_io", "turns", sessionKey(session)+".json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			stopTurn(map[string]any{"session_id": session}, time.Now())
			if got := readOnlyContextEvent(t, state)["payload"].(map[string]any)["unknown_reason"]; got != tc.want {
				t.Fatalf("unknown_reason = %v, want %s", got, tc.want)
			}
		})
	}
	t.Run("corrupt-read-resets", func(t *testing.T) {
		state, repo, session := t.TempDir(), filepath.Join(t.TempDir(), "repo"), "journal-corrupt-read"
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PURSER_STATE_DIR", state)
		path := filepath.Join(state, "context_io", "turns", sessionKey(session)+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"observations":"corrupt","read_count":[]}`), 0600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(repo, "notes.txt")
		if err := os.WriteFile(target, []byte("read after corrupt state\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": target}}, time.Now()); err != nil {
			t.Fatal(err)
		}
		turn, reason := loadTurn(path)
		if reason != "" || turn == nil || turn["unknown_reason"] != "CORRUPT_STATE" || number(turn["read_count"]) != 1 {
			t.Fatalf("reset corrupt journal = %#v, %q", turn, reason)
		}
	})
	t.Run("stale", func(t *testing.T) {
		state, session := t.TempDir(), "journal-stale"
		t.Setenv("PURSER_STATE_DIR", state)
		path := filepath.Join(state, "context_io", "turns", sessionKey(session)+".json")
		turn := newTurn("")
		turn["opened_at"] = float64(time.Now().Add(-7*time.Hour).UnixNano()) / 1e9
		if err := writeTurn(path, turn); err != nil {
			t.Fatal(err)
		}
		stopTurn(map[string]any{"session_id": session}, time.Now())
		if got := readOnlyContextEvent(t, state)["payload"].(map[string]any)["unknown_reason"]; got != "STALE_STATE" {
			t.Fatalf("unknown_reason = %v, want STALE_STATE", got)
		}
	})
}

func TestContextTelemetryPrivacyPersistenceAndConcurrentStop(t *testing.T) {
	state, repo, session := t.TempDir(), filepath.Join(t.TempDir(), "repo-private"), "private-session-identifier"
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", state)
	UserPromptSubmit(map[string]any{"session_id": session, "cwd": repo}, time.Now())
	target := filepath.Join(repo, "private-name.txt")
	if err := os.WriteFile(target, []byte("PROMPT_AND_CONTENT_SENTINEL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Read(map[string]any{"session_id": session, "cwd": repo, "tool_input": map[string]any{"file_path": target}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(state, "context_io", "turns", sessionKey(session)+".json")
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PROMPT_AND_CONTENT_SENTINEL", target, filepath.Base(target), filepath.Base(repo), session} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("journal exposed %q", forbidden)
		}
	}
	if !strings.Contains(string(data), "identity") {
		t.Fatal("journal omitted file identity")
	}
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); stopTurn(map[string]any{"session_id": session}, time.Now()) }()
	}
	workers.Wait()
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("journal remains after Stop: %v", err)
	}
	event := readOnlyContextEvent(t, state)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PROMPT_AND_CONTENT_SENTINEL", target, filepath.Base(target), filepath.Base(repo), "identity", "st_dev", "st_ino"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("event exposed %q", forbidden)
		}
	}
	canonical := filepath.Join(state, "telemetry", "events", event["ts_utc"].(string)[:7], "session_"+session+".jsonl")
	lines, err := os.ReadFile(canonical)
	if err != nil || strings.Count(strings.TrimSpace(string(lines)), "\n") != 0 {
		t.Fatalf("expected exactly one event record, got %q, %v", lines, err)
	}
}

func readOnlyContextEvent(t *testing.T, root string) map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "telemetry", "events", "????-??", "session_*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("event files = %v, %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &event); err != nil {
		t.Fatal(err)
	}
	return event
}
