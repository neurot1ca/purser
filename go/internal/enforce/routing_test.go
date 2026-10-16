package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
		if !strings.HasPrefix(item.Name, "p40:") {
			continue
		}
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

func TestCoderSelectionParityP41(t *testing.T) {
	fixtureBytes, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/routing.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name      string         `json:"name"`
			Selection map[string]any `json:"selection"`
			Selected  map[string]any `json:"selected"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(fixtureBytes))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		if !strings.HasPrefix(item.Name, "p41:") {
			continue
		}
		t.Run(item.Name, func(t *testing.T) {
			input := item.Selection
			config, _ := input["config"].(map[string]any)
			toolInput, _ := input["tool_input"].(map[string]any)
			live, _ := input["live_thread"].(map[string]any)
			active, _ := input["active"].(map[string]any)
			candidates, _ := input["candidates"].([]any)
			got := SelectCoder(stringValue(input["route"]), stringValue(input["model"]), stringValue(input["effort"]),
				stringValue(input["reason"]), input["new_call"] == true, input["reply_call"] == true,
				config, toolInput, live, active, candidates, false)
			var actual map[string]any
			data, _ := json.Marshal(got)
			if err := json.Unmarshal(data, &actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, item.Selected) {
				t.Fatalf("selection = %#v, want %#v", actual, item.Selected)
			}
		})
	}
}

func TestGearSelectionPreservesAuthorityBoundaries(t *testing.T) {
	config := map[string]any{
		"coder_agent": "codex",
		"routes": map[string]any{
			"mechanical": map[string]any{"model": "gpt-6.1-luna", "effort": "high"},
			"normal":     map[string]any{"model": "gpt-6.1-sol", "effort": "medium"},
		},
		"fallback": map[string]any{"agent": "agy", "model": "claude-test"},
	}
	input := map[string]any{"model": "gpt-6.1-luna", "config": map[string]any{"model_reasoning_effort": "high"}}
	selected := SelectCoder("mechanical", "gpt-6-sol", "medium", "", true, false, config, input, nil, nil, nil, true)
	if selected.Model != "gpt-6.1-sol" || selected.Effort != "medium" || selected.ModelSource != "route" {
		t.Fatalf("explicit override bypassed gear: %#v", selected)
	}
	reply := SelectCoder("mechanical", "gpt-6.1-luna", "high", "", false, true, config, nil,
		map[string]any{"agent": "codex", "model": "recorded-thread-model", "effort": "xhigh"}, nil, nil, true)
	if reply.Model != "recorded-thread-model" || reply.Effort != "xhigh" {
		t.Fatalf("reply model identity changed: %#v", reply)
	}
	nonCodex := map[string]any{"coder_agent": "opencode", "routes": config["routes"], "fallback": config["fallback"]}
	alt := SelectCoder("mechanical", "gpt-6.1-luna", "high", "", true, false, nonCodex, nil, nil, nil,
		[]any{map[string]any{"id": "vendor/model", "status": "measured"}}, true)
	if alt.Agent != "opencode" || alt.Model != "vendor/model" {
		t.Fatalf("gear changed non-Codex candidate: %#v", alt)
	}
	fallback := SelectCoder("mechanical", "gpt-6-sol", "medium", "", true, false, config, input, nil,
		map[string]any{"active_coder": "fallback"}, nil, true)
	if fallback.Agent != "agy" || fallback.Model != "claude-test" {
		t.Fatalf("gear changed fallback selection: %#v", fallback)
	}
}

func TestMechanicalRaceDispatchKeepsExactMeasuredID(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	cache, err := json.Marshal(map[string]any{
		"checked_on": time.Now().UTC().Format("2006-01-02"),
		"agents":     map[string][]string{"opencode": {"vendor/model-2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state, "model-resolution.json"), cache, 0600); err != nil {
		t.Fatal(err)
	}
	selection := SelectCoder("mechanical", "route-model", "none", "", true, false,
		map[string]any{"coder_agent": "codex", "mechanical_opencode_enabled": true}, nil, nil, nil,
		[]any{
			map[string]any{"id": "vendor/model-1", "status": "measured", "mechanical_samples": []any{true, true, true, true, true}},
			map[string]any{"id": "vendor/model-2", "status": "unmeasured", "mechanical_samples": []any{}},
		}, false)
	if selection.Agent != "opencode" || selection.Model != "vendor/model-1" {
		t.Fatalf("race dispatch = %#v, measured candidate inherited a newer catalog ID", selection)
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
