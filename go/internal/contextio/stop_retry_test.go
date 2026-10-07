package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPythonStopRetryGoldenParity(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "stop_retry.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name    string           `json:"name"`
			Records []map[string]any `json:"records"`
			Index   map[string]any   `json:"index"`
			JobRaw  *string          `json:"job_raw"`
			Want    string           `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "compaction_advisor._codex_retry_reason" {
		t.Fatalf("invalid Python retry fixture: %v", err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			repository := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(repository, 0700); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "state")
			t.Setenv("PURSER_STATE_DIR", root)
			if test.Index != nil {
				index := test.Index
				if index["project_key"] == "__PROJECT_KEY__" {
					index["project_key"] = repositoryProjectKey(resolve(repository))
				}
				id := str(index["jobId"])
				indexDir := filepath.Join(root, "async_jobs")
				if err := os.MkdirAll(indexDir, 0700); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(indexDir, id+".json"), encoded, 0600); err != nil {
					t.Fatal(err)
				}
				jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
				if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
					t.Fatal(err)
				}
				if test.JobRaw == nil {
					t.Fatal("job record missing")
				}
				if err := os.WriteFile(jobPath, []byte(*test.JobRaw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := codexRetryReason(test.Records, 0, repository); got != test.Want {
				t.Fatalf("codexRetryReason = %q, want %q", got, test.Want)
			}
		})
	}
}

func TestStopRetrySkipsTerminalFailedAsyncIndex(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repository, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("PURSER_STATE_DIR", root)
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	index := map[string]any{"jobId": id, "call": "call-1", "project_key": repositoryProjectKey(resolve(repository)), "stage": "FAILED"}
	indexDir := filepath.Join(root, "async_jobs")
	if err := os.MkdirAll(indexDir, 0700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(indexDir, id+".json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	job := `{"jobId":"` + id + `","call":"call-1","state":"SUCCEEDED","label":"Build result"}`
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
	if err = os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(jobPath, []byte(job), 0600); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{
		{"type": "user", "message": map[string]any{"content": "Continue."}},
		{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "mcp__plugin_purser_codex-main__codex_start", "input": map[string]any{}}}}},
		{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": `{"jobId":"` + id + `"}`}}}},
	}
	if got := codexRetryReason(records, 0, repository); got != "" {
		t.Fatalf("terminal failed job produced Stop retry text: %q", got)
	}
}

func TestPythonStopRetryStateWriteParity(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "stop_retry.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		StateWrite struct {
			Oracle string `json:"oracle"`
			Cases  []struct {
				Name            string           `json:"name"`
				Records         []map[string]any `json:"records"`
				Index           map[string]any   `json:"index"`
				JobRaw          string           `json:"job_raw"`
				InitialCount    any              `json:"initial_count"`
				InitialPending  any              `json:"initial_pending"`
				ExpectedBlock   bool             `json:"expected_block"`
				ExpectedCount   int              `json:"expected_count"`
				ExpectedPending any              `json:"expected_pending"`
			} `json:"cases"`
			Prompt struct {
				Context      string `json:"context"`
				InputPending string `json:"input_pending"`
				Count        int    `json:"count"`
				Pending      any    `json:"pending"`
			} `json:"prompt"`
		} `json:"state_write"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.StateWrite.Oracle != "compaction_advisor.stop retry state and prompt_retry_context" {
		t.Fatalf("invalid Python retry state fixture: %v", err)
	}
	for _, test := range fixture.StateWrite.Cases {
		t.Run(test.Name, func(t *testing.T) {
			repository := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(repository, 0700); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "state")
			t.Setenv("PURSER_STATE_DIR", root)
			if test.Index != nil {
				index := test.Index
				id := str(index["jobId"])
				if index["project_key"] == "__PROJECT_KEY__" {
					index["project_key"] = repositoryProjectKey(resolve(repository))
				}
				indexDir := filepath.Join(root, "async_jobs")
				if err := os.MkdirAll(indexDir, 0700); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(indexDir, id+".json"), encoded, 0600); err != nil {
					t.Fatal(err)
				}
				jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
				if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(jobPath, []byte(test.JobRaw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			statePath := filepath.Join(root, "projects", repositoryProjectKey(resolve(repository)), "telemetry", "compaction_advisor.json")
			initial, err := json.Marshal(map[string]any{"session_id": "session", "codex_retry_count": test.InitialCount,
				"codex_retry_pending": test.InitialPending})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, initial, 0600); err != nil {
				t.Fatal(err)
			}
			reason := codexRetryReason(test.Records, 0, repository)
			gotBlock := persistCodexRetry(statePath, "session", reason) != ""
			state := readAdvisorState(statePath, "session")
			if gotBlock != test.ExpectedBlock || number(state["codex_retry_count"]) != test.ExpectedCount || !sameJSONValue(state["codex_retry_pending"], test.ExpectedPending) {
				t.Fatalf("retry state = block:%v count:%v pending:%#v, want block:%v count:%d pending:%#v", gotBlock, state["codex_retry_count"], state["codex_retry_pending"], test.ExpectedBlock, test.ExpectedCount, test.ExpectedPending)
			}
		})
	}

	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	statePath := filepath.Join(root, "projects", "prompt", "telemetry", "compaction_advisor.json")
	initial, _ := json.Marshal(map[string]any{"session_id": "prompt-retry", "codex_retry_count": 2,
		"codex_retry_pending": fixture.StateWrite.Prompt.InputPending})
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, initial, 0600); err != nil {
		t.Fatal(err)
	}
	if got := promptRetryContext(statePath, "prompt-retry"); got != fixture.StateWrite.Prompt.Context {
		t.Fatalf("prompt retry context = %q, want %q", got, fixture.StateWrite.Prompt.Context)
	}
	if got := promptRetryContext(statePath, "prompt-retry"); got != "" {
		t.Fatalf("prompt retry context repeated: %q", got)
	}
	state := readAdvisorState(statePath, "prompt-retry")
	if number(state["codex_retry_count"]) != fixture.StateWrite.Prompt.Count || state["codex_retry_pending"] != fixture.StateWrite.Prompt.Pending {
		t.Fatalf("prompt retry state = %#v", state)
	}
}

func sameJSONValue(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
