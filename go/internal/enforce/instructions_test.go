package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstructionsParityFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/instructions.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle    string `json:"oracle"`
		FullSuite []struct {
			Name, Policy, Prompt       string
			Acceptance, Asks, Required bool
		} `json:"full_suite_cases"`
		Append []struct {
			Name, Prompt, Expected string
			FullSuite              bool `json:"full_suite"`
		} `json:"append_cases"`
		Rewrite []struct {
			Name, Phase, Agent string
			NewCall            bool `json:"new_call"`
			FullSuite          bool `json:"full_suite"`
			Input, Expected    map[string]any
		} `json:"rewrite_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "Python codex.py pass 42 rewrite and instruction helpers" {
		t.Fatalf("fixture: %v %q", err, fixture.Oracle)
	}
	for _, item := range fixture.FullSuite {
		t.Run(item.Name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(item.Policy), 0600); err != nil {
				t.Fatal(err)
			}
			if got := ClaudeRunsFullSuiteAtAcceptance(repo); got != item.Acceptance {
				t.Errorf("acceptance detection = %v, want %v", got, item.Acceptance)
			}
			if got := AsksWorkerToRunFullSuite(item.Prompt); got != item.Asks {
				t.Errorf("request detection = %v, want %v", got, item.Asks)
			}
			if got := ClaudeRunsFullSuiteAtAcceptance(repo) && AsksWorkerToRunFullSuite(item.Prompt); got != item.Required {
				t.Errorf("combined requirement = %v, want %v", got, item.Required)
			}
		})
	}
	for _, item := range fixture.Append {
		t.Run(item.Name, func(t *testing.T) {
			if got := AppendWorkerValidationInstruction(item.Prompt, item.FullSuite); got != item.Expected {
				t.Fatalf("instruction bytes differ\n got: %q\nwant: %q", got, item.Expected)
			}
		})
	}
	for _, item := range fixture.Rewrite {
		t.Run(item.Name, func(t *testing.T) {
			got := RewriteInstructionInput(item.Input, item.Phase, item.NewCall, item.Agent, item.FullSuite)
			if !reflect.DeepEqual(got, item.Expected) {
				t.Fatalf("rewritten input differs\n got: %#v\nwant: %#v", got, item.Expected)
			}
		})
	}
}

func TestFullSuiteGuardRequiresImperativeRequest(t *testing.T) {
	if got := WorkerFullSuiteRequestPhrase("ürun all tests"); got != "" {
		t.Fatalf("non-ASCII letter glued to imperative matched %q", got)
	}
	if got := WorkerFullSuiteRequestPhrase("All tests write only to temporary directories."); got != "" {
		t.Fatalf("descriptive test sentence matched %q", got)
	}
	if got := WorkerFullSuiteRequestPhrase("Run all tests, then report."); got != "Run all tests" {
		t.Fatalf("imperative request phrase = %q", got)
	}
}
