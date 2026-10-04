package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"purser-hook/internal/ledger"
)

func TestRoutingParityP40(t *testing.T) {
	fixtureBytes, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/routing.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name          string         `json:"name"`
			Kind          string         `json:"kind"`
			ThreadID      string         `json:"thread_id"`
			TaskID        string         `json:"task_id"`
			Prompt        string         `json:"prompt"`
			Label         string         `json:"label"`
			RecordedTask  string         `json:"recorded_task"`
			RecordedPhase string         `json:"recorded_phase"`
			RecordedRoute string         `json:"recorded_route"`
			Phase         string         `json:"phase"`
			Route         string         `json:"route"`
			Expected      string         `json:"expected"`
			Input         map[string]any `json:"input"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	configBytes, err := os.ReadFile("../../../.claude/hooks/orchestrator/config.json")
	if err != nil {
		t.Fatal(err)
	}
	configDecoder := json.NewDecoder(bytes.NewReader(configBytes))
	configDecoder.UseNumber()
	if err = configDecoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			temp := t.TempDir()
			root := filepath.Join(temp, "repo")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			runtimeRoot := filepath.Join(temp, "runtime")
			rootKey := projectKey(&root)
			state := filepath.Join(runtimeRoot, "projects", rootKey)
			if item.Kind == "corrupt" {
				path, _ := ledger.PathForTask(state, item.TaskID)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				task := item.TaskID
				if item.RecordedTask != "" {
					task = item.RecordedTask
				}
				threadID := item.ThreadID
				options := map[string]any{"phase": valueOr(item.RecordedPhase, "operation"), "scope": []any{"src"}}
				if item.Label != "" {
					options["label"] = item.Label
				}
				record := ledger.BuildSessionRecord(task, &threadID, valueOr(item.RecordedRoute, "operation"), "model", "high", options)
				if saved := ledger.Save(state, record); saved.Status != ledger.OK {
					t.Fatalf("save fixture: %#v", saved)
				}
			}
			prompt := item.Prompt
			if item.Kind == "mismatch" {
				prompt = "ORCH|task=" + item.TaskID + "|phase=" + item.Phase + "|route=" + item.Route + "\nContinue."
			}
			toolInput := map[string]any{"threadId": item.ThreadID, "prompt": prompt}
			for key, value := range item.Input {
				toolInput[key] = value
			}
			event := map[string]any{"tool_input": toolInput}
			got := RecoverReply(event, runtimeRoot, &root, config)
			if got.Code != item.Expected {
				t.Fatalf("recovery code = %q, want %q (%s)", got.Code, item.Expected, got.Message)
			}
			if item.Expected == "VALID" && (got.Label == nil || *got.Label != item.Label) {
				t.Fatalf("inherited label = %v, want %q", got.Label, item.Label)
			}
		})
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
