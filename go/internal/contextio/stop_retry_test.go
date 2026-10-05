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
