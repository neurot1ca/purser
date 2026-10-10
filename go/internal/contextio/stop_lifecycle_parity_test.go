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

func TestP132StopLifecycleBehavior(t *testing.T) {
	path := filepath.Join("testdata", "stop_lifecycle_python_fixture.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name         string           `json:"name"`
			Session      string           `json:"session"`
			Transcript   *string          `json:"transcript"`
			Events       []map[string]any `json:"events"`
			Outputs      []map[string]any `json:"outputs"`
			Snapshot     map[string]any   `json:"snapshot"`
			PromptOutput map[string]any   `json:"prompt_output"`
			AfterPrompt  map[string]any   `json:"after_prompt"`
			ActiveSeed   map[string]any   `json:"active_seed"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("invalid lifecycle fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root, repo, home := t.TempDir(), filepath.Join(t.TempDir(), "repo"), t.TempDir()
			projects := filepath.Join(home, ".claude", "projects")
			if err := os.MkdirAll(projects, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PURSER_STATE_DIR", root)
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
			var transcript string
			if tc.Transcript != nil {
				transcript = filepath.Join(projects, tc.Session+".jsonl")
				if err := os.WriteFile(transcript, []byte(*tc.Transcript), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.ActiveSeed != nil {
				transcriptData, err := os.ReadFile(transcript)
				if err != nil {
					t.Fatal(err)
				}
				if offset, ok := tc.ActiveSeed["anchor_search_start_offset"].(float64); ok {
					if boundary := strings.IndexByte(string(transcriptData), '\n') + 1; int(offset) != boundary {
						t.Fatalf("fixture anchor offset %d does not match Go transcript boundary %d", int(offset), boundary)
					}
				}
				task := cloneMap(tc.ActiveSeed)
				task["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				task["project_key"] = p132ProjectKey(repo)
				task["canonical_transcript_path"] = transcript
				activePath := filepath.Join(root, "active_tasks", tc.Session+".json")
				if err := os.MkdirAll(filepath.Dir(activePath), 0700); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(task)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(activePath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			finalizeCalls := 0
			contextio.SetStopTaskFinalizer(func(root, session, transcriptPath string) {
				finalizeCalls++
				enforce.FinalizeStopTask(root, session, transcriptPath)
			})
			t.Cleanup(func() { contextio.SetStopTaskFinalizer(enforce.FinalizeStopTask) })
			expectedFinalizeCalls := 0
			for i, saved := range tc.Events {
				event := cloneMap(saved)
				event["cwd"] = repo
				if _, ok := event["transcript_path"]; ok {
					event["transcript_path"] = transcript
				}
				if session, ok := event["session_id"].(string); ok && session != "" && event["stop_hook_active"] != true {
					expectedFinalizeCalls++
				}
				got := decodeStop(t, contextio.Stop(event, time.Now()))
				if !reflect.DeepEqual(got, tc.Outputs[i]) {
					t.Fatalf("Stop[%d] = %#v, want %#v", i, got, tc.Outputs[i])
				}
			}
			if finalizeCalls != expectedFinalizeCalls {
				t.Fatalf("Stop finalizer calls = %d, want %d", finalizeCalls, expectedFinalizeCalls)
			}
			gotSnapshot := normalizeP132Snapshot(p132Snapshot(t, root, repo, tc.Session), repo)
			if !reflect.DeepEqual(gotSnapshot, tc.Snapshot) {
				t.Fatalf("state = %#v, want %#v", gotSnapshot, tc.Snapshot)
			}
			if tc.PromptOutput != nil {
				got := decodeStop(t, contextio.UserPromptSubmit(map[string]any{"hook_event_name": "UserPromptSubmit", "cwd": repo,
					"session_id": tc.Session, "prompt": "Continue."}, time.Now()))
				if !reflect.DeepEqual(got, tc.PromptOutput) {
					t.Fatalf("prompt output = %#v, want %#v", got, tc.PromptOutput)
				}
				gotSnapshot = normalizeP132Snapshot(p132Snapshot(t, root, repo, tc.Session), repo)
				if !reflect.DeepEqual(gotSnapshot, tc.AfterPrompt) {
					t.Fatalf("post-prompt state = %#v, want %#v", gotSnapshot, tc.AfterPrompt)
				}
			}
		})
	}
}

func TestP132GoWrittenActiveRecordFinalization(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "stop_lifecycle_python_fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name       string         `json:"name"`
			Session    string         `json:"session"`
			Transcript *string        `json:"transcript"`
			ActiveSeed map[string]any `json:"active_seed"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var tc *struct {
		Name       string         `json:"name"`
		Session    string         `json:"session"`
		Transcript *string        `json:"transcript"`
		ActiveSeed map[string]any `json:"active_seed"`
	}
	for i := range fixture.Cases {
		if fixture.Cases[i].Name == "active-stop" {
			tc = &fixture.Cases[i]
			break
		}
	}
	if tc == nil || tc.Transcript == nil || tc.ActiveSeed == nil {
		t.Fatal("active task fixture is incomplete")
	}
	root, repo, home := t.TempDir(), filepath.Join(t.TempDir(), "repo"), t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	projects := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projects, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projects, tc.Session+".jsonl")
	if err := os.WriteFile(transcript, []byte(*tc.Transcript), 0600); err != nil {
		t.Fatal(err)
	}
	record := cloneMap(tc.ActiveSeed)
	record["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["project_key"] = p132ProjectKey(repo)
	canonicalTranscript := transcript
	if runtime.GOOS == "windows" {
		canonicalTranscript = strings.ToLower(canonicalTranscript)
	}
	record["canonical_transcript_path"] = canonicalTranscript
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(root, "active_tasks", tc.Session+".json")
	if err := os.MkdirAll(filepath.Dir(activePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	enforce.FinalizeStopTask(root, tc.Session, transcript)
	var finalized map[string]any
	data, err = os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &finalized); err != nil {
		t.Fatal(err)
	}
	if finalized["completed"] != true || finalized["invalidated"] != false {
		t.Fatalf("Go finalizer state = %#v", finalized)
	}
}

func decodeStop(t *testing.T, data []byte) map[string]any {
	t.Helper()
	if len(data) == 0 {
		return map[string]any{}
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func cloneMap(input map[string]any) map[string]any {
	data, _ := json.Marshal(input)
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	return result
}

func p132Snapshot(t *testing.T, root, repo, session string) map[string]any {
	t.Helper()
	key := p132ProjectKey(repo)
	advisorPath := filepath.Join(root, "projects", key, "telemetry", "compaction_advisor.json")
	advisor := map[string]any{}
	if data, err := os.ReadFile(advisorPath); err == nil {
		_ = json.Unmarshal(data, &advisor)
	}
	activePath := filepath.Join(root, "active_tasks", session+".json")
	var active map[string]any
	if data, err := os.ReadFile(activePath); err == nil {
		_ = json.Unmarshal(data, &active)
	}
	taskEventCount := 0
	if taskID, ok := active["task_id"].(string); ok && taskID != "" {
		eventPath := filepath.Join(root, "telemetry", "events", "by_task", taskID+".jsonl")
		if data, err := os.ReadFile(eventPath); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var event map[string]any
				if json.Unmarshal([]byte(line), &event) == nil && event["event_type"] == "CLAUDE_TASK_USAGE_RECORDED" {
					taskEventCount++
				}
			}
		}
	}
	var activeSnapshot any
	if active != nil {
		normalized := cloneMap(active)
		normalized["canonical_transcript_path"] = "<TRANSCRIPT>"
		normalized["created_at"] = "<CREATED_AT>"
		normalized["project_key"] = "<PROJECT_KEY>"
		for _, field := range []string{"transcript_dev", "transcript_ino", "transcript_ctime_ns"} {
			delete(normalized, field)
		}
		activeSnapshot = normalized
	}
	count, pending := any(float64(0)), any(false)
	if value, ok := advisor["codex_retry_count"]; ok {
		count = value
	}
	if value, ok := advisor["codex_retry_pending"]; ok {
		pending = value
	}
	var completed, invalidated any
	var reason any
	if active != nil {
		completed, invalidated, reason = active["completed"], active["invalidated"], active["invalidated_reason"]
	}
	return map[string]any{"retry_count": count, "retry_pending": pending, "active_completed": completed,
		"active_invalidated": invalidated, "active_reason": reason, "advisor_state": advisor, "active_state": activeSnapshot,
		"task_event_count": float64(taskEventCount)}
}

func normalizeP132Snapshot(value any, repo string) map[string]any {
	return normalizeP132Value(value, repo).(map[string]any)
}

func normalizeP132Value(value any, repo string) any {
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			if key == "project_key" {
				out[key] = "<PROJECT_KEY>"
			} else {
				out[key] = normalizeP132Value(child, repo)
			}
		}
		return out
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			out[i] = normalizeP132Value(child, repo)
		}
		return out
	case string:
		escapedRepo := strings.ReplaceAll(repo, `\`, `\\`)
		item = strings.ReplaceAll(item, escapedRepo, "<REPO>")
		item = strings.ReplaceAll(item, repo, "<REPO>")
		if strings.Contains(item, "<REPO>") {
			item = strings.ReplaceAll(item, `\`, "/")
		}
		return item
	default:
		return value
	}
}

func p132ProjectKey(repo string) string {
	project := repo
	if runtime.GOOS == "windows" {
		project = strings.ToLower(project)
	}
	sum := sha256.Sum256([]byte(project))
	return hex.EncodeToString(sum[:])[:16]
}
