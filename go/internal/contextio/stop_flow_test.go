package contextio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPythonStopFlowGoldenParity(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "stop_flow.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name                string           `json:"name"`
			Records             []map[string]any `json:"records"`
			RawTranscript       string           `json:"raw_transcript"`
			BlockedUser         int              `json:"blocked_user"`
			HasReasons          bool             `json:"has_reasons"`
			Expected            string           `json:"expected"`
			RetryExpected       string           `json:"retry_expected"`
			ExpectedBlockedUser int              `json:"expected_blocked_user"`
		} `json:"cases"`
		Ordering struct {
			Retry    string   `json:"retry"`
			Advisor  []string `json:"advisor"`
			Flow     string   `json:"flow"`
			Expected []string `json:"expected"`
		} `json:"ordering"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "compaction_advisor.stop" {
		t.Fatalf("invalid Stop flow fixture: %v", err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			records := test.Records
			if test.RawTranscript != "" {
				path := filepath.Join(t.TempDir(), "transcript.jsonl")
				if err := os.WriteFile(path, []byte(test.RawTranscript), 0600); err != nil {
					t.Fatal(err)
				}
				records = readTranscriptRecords(path)
			}
			latestUser := latestGenuineUser(records)
			got, blocked := stopFlowReason(records, latestUser, test.BlockedUser, test.HasReasons)
			if got != test.Expected || blocked != test.ExpectedBlockedUser {
				t.Fatalf("stopFlowReason = (%q,%d), want (%q,%d)", got, blocked, test.Expected, test.ExpectedBlockedUser)
			}
			t.Setenv("PURSER_STATE_DIR", t.TempDir())
			if retry := codexRetryReason(records, latestUser, t.TempDir()); retry != test.RetryExpected {
				t.Fatalf("codexRetryReason = %q, want %q", retry, test.RetryExpected)
			}
		})
	}
	if got := stopReasons(fixture.Ordering.Retry, fixture.Ordering.Advisor, fixture.Ordering.Flow); !reflect.DeepEqual(got, fixture.Ordering.Expected) {
		t.Fatalf("Stop reason order = %#v, want %#v", got, fixture.Ordering.Expected)
	}
}

func TestPlanCodexDispatchedDetectsDispatcher(t *testing.T) {
	records := func(subagentType string) []map[string]any {
		return []map[string]any{
			{"type": "user", "message": map[string]any{"content": []any{"continue"}}},
			{"type": "assistant", "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "name": "mcp__Agent", "input": map[string]any{"subagent_type": subagentType}},
			}}},
		}
	}

	tests := []struct {
		name         string
		subagentType string
		want         bool
	}{
		{name: "dispatcher without plan identity", subagentType: "purser:dispatcher", want: false},
		{name: "other subagent after latest user", subagentType: "general-purpose", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := planCodexDispatched(records(test.subagentType), 0, "plan-123"); got != test.want {
				t.Fatalf("planCodexDispatched() = %t, want %t", got, test.want)
			}
		})
	}
	identified := []map[string]any{
		{"type": "user", "message": map[string]any{"content": []any{"continue"}}},
		{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "mcp__Agent", "input": map[string]any{"subagent_type": "purser:dispatcher", "prompt": "ORCH|task=plan-123-m1of2-p1c|phase=implement"}},
		}}},
	}
	if !planCodexDispatched(identified, 0, "plan-123") || planCodexDispatched(identified, 0, "plan-456") {
		t.Fatal("dispatcher suppression did not follow the identified plan")
	}
}

func latestGenuineUser(records []map[string]any) int {
	latest := -1
	for i, record := range records {
		if genuineTranscriptUser(record) {
			latest = i
		}
	}
	return latest
}
