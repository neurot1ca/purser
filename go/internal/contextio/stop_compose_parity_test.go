package contextio_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
)

var _ = enforce.FinalizeStopTask

func TestP131StopCompositionFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "stop_compose.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string         `json:"name"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "registered Python codex.stop + compaction_advisor.stop" {
		t.Fatalf("invalid Stop composition fixture: %v", err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root, repo, home := t.TempDir(), filepath.Join(t.TempDir(), "repo"), t.TempDir()
			projects := filepath.Join(home, ".claude", "projects")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(projects, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PURSER_STATE_DIR", root)
			t.Setenv("USERPROFILE", home)
			session := "p131-" + test.Name
			transcript := filepath.Join(projects, session+".jsonl")
			records := []byte(composeTranscript(test.Name))
			active := test.Expected["active_completed"] != nil
			if active {
				records = append(records, []byte(`{"type":"assistant","requestId":"task-request","message":{"id":"task-message","content":[{"type":"tool_use","id":"task-tool"}],"usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":1,"cache_read_input_tokens":2}}}`+"\n")...)
			}
			writeComposeTranscript(t, transcript, records)
			activePath := filepath.Join(root, "active_tasks", session+".json")
			if active {
				canonical := transcript
				if test.Name == "finalization-failure-isolated" {
					canonical = filepath.Join(projects, "wrong.jsonl")
				}
				writeComposeJSON(t, activePath, map[string]any{"task_id": "task-" + test.Name, "tool_use_id": "task-tool",
					"project_key": "0123456789abcdef", "canonical_transcript_path": canonical, "initial_size": int64(0),
					"anchor_byte_offset": int64(0), "anchor_search_start_offset": int64(0), "anchor_deferred": false,
					"created_at": time.Now().UTC().Format(time.RFC3339Nano), "completed": false, "invalidated": false})
			}
			var unlock func() error
			if test.Name == "concurrent-finalization-contention" {
				unlock, err = contextio.LockFile(filepath.Join(root, "active_tasks", ".locks", session+".lock"))
				if err != nil {
					t.Fatal(err)
				}
			}
			out := contextio.Stop(map[string]any{"cwd": repo, "session_id": session, "stop_hook_active": false,
				"last_assistant_message": "Done.", "transcript_path": transcript}, time.Now())
			if unlock != nil {
				_ = unlock()
			}
			var decision map[string]any
			if len(out) > 0 {
				if err := json.Unmarshal(out, &decision); err != nil {
					t.Fatal(err)
				}
			}
			gotDecision, gotReason := "allow", ""
			if decision != nil {
				gotDecision, _ = decision["decision"].(string)
				gotReason, _ = decision["reason"].(string)
			}
			project := repo
			if runtime.GOOS == "windows" {
				project = strings.ToLower(project)
			}
			sum := sha256.Sum256([]byte(project))
			advisorPath := filepath.Join(root, "projects", hex.EncodeToString(sum[:])[:16], "telemetry", "compaction_advisor.json")
			var advisor, task map[string]any
			if b, e := os.ReadFile(advisorPath); e == nil {
				_ = json.Unmarshal(b, &advisor)
			}
			if b, e := os.ReadFile(activePath); e == nil {
				_ = json.Unmarshal(b, &task)
			}
			count := 0.0
			if n, ok := advisor["codex_retry_count"].(float64); ok {
				count = n
			}
			var marker any
			if _, e := os.Stat(filepath.Join(root, "active_tasks", session+".contention")); e == nil {
				marker = true
			} else {
				marker = false
			}
			got := map[string]any{"decision": gotDecision, "reason": gotReason, "retry_count": count, "contention": marker}
			for key, value := range map[string]string{"active_completed": "completed", "active_invalidated": "invalidated", "active_reason": "invalidated_reason"} {
				got[key] = nil
				if task != nil {
					got[key] = task[value]
				}
			}
			if !reflect.DeepEqual(got, test.Expected) {
				t.Fatalf("got %#v, want %#v", got, test.Expected)
			}
		})
	}
}

func composeTranscript(name string) string {
	switch name {
	case "retry-block", "finalization-success-retry-block":
		return `{"type":"user","message":{"content":"Try Codex."}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"call-1","name":"mcp__plugin_purser_codex-main__codex_start","input":{}}]}}` + "\n" +
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","is_error":true,"content":"classifier denied permission"}]}}` + "\n"
	case "flow-question-block":
		return `{"type":"user","message":{"content":"Continue."}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"result-1","name":"mcp__plugin_purser_codex-main__codex_result","input":{}}]}}` + "\n" +
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"result-1","content":"Finished"}]}}` + "\n" +
			`{"type":"assistant","message":{"content":"Should I continue?"}}` + "\n"
	default:
		return `{"type":"user","message":{"content":"Continue."}}` + "\n" + `{"type":"assistant","message":{"content":"Done."}}` + "\n"
	}
}

func writeComposeJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(f).Encode(value); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeComposeTranscript(t *testing.T, path string, records []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, records, 0600); err != nil {
		t.Fatal(err)
	}
}
