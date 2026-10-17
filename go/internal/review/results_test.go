package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResultsGolden(t *testing.T) {
	paths, err := filepath.Glob("testdata/results/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if filepath.Base(path) == "mappings.json" {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Adapter         string         `json:"adapter"`
				Stdout          string         `json:"stdout"`
				IncludeMetadata bool           `json:"include_metadata"`
				Expected        map[string]any `json:"expected"`
			}
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			got, err := ParseEnvelope(fixture.Adapter, []byte(fixture.Stdout), fixture.IncludeMetadata)
			if expectedError, ok := fixture.Expected["error_type"]; ok {
				if err == nil {
					t.Fatalf("expected oracle error %v, got %#v", expectedError, got)
				}
				if got := Unavailable("REVIEWER_RESPONSE_INVALID", err.Error(), map[string]any{"reviewer1_model_requested": "wanted/model"}, fixture.Adapter).Map(); got["error_code"] != "REVIEWER_RESPONSE_INVALID" {
					t.Fatalf("invalid result = %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got.Map())
			wantJSON, _ := json.Marshal(fixture.Expected)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("parsed = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestCodexEventStreamParsesFinalAgentReview(t *testing.T) {
	stdout := "{\"type\":\"thread.started\",\"thread_id\":\"01a12f70-f454-7ba0-9ccb-259e58d05bcd\"}\n" +
		"{\"type\":\"turn.started\"}\n" +
		"{\"type\":\"item.completed\",\"item\":{\"id\":\"item_0\",\"type\":\"agent_message\",\"text\":\"{\\\"result\\\":\\\"UNCERTAIN\\\",\\\"validation\\\":\\\"UNKNOWN\\\",\\\"findings\\\":[{\\\"code\\\":\\\"SCOPE\\\",\\\"message\\\":\\\"The packet does not include enough context to assess the change.\\\"}]}\"}}\n" +
		"{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":18850,\"output_tokens\":73}}\n"
	parsed, err := ParseCodexEnvelope([]byte(stdout), true)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ConversationID != "01a12f70-f454-7ba0-9ccb-259e58d05bcd" || parsed.Response["result"] != Uncertain {
		t.Fatalf("parsed Codex review = %#v", parsed)
	}
}

func TestOpenCodeEnvelopeFailedToolDoesNotInvalidateCompletedResponse(t *testing.T) {
	valid := `{"result":"NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE","validation":"PASS","findings":[]}`
	failedTool := `{"type":"tool_use","part":{"state":{"status":"error"}}}`
	completion := `{"type":"step_finish","part":{"reason":"stop"}}`
	textEvent := func(text string) string {
		data, _ := json.Marshal(map[string]any{"type": "text", "part": map[string]any{"text": text}})
		return string(data)
	}
	tests := []struct {
		name    string
		stream  string
		wantErr bool
	}{
		{name: "failed tool then valid completion", stream: failedTool + "\n" + textEvent(valid) + "\n" + completion},
		{name: "failed tool without completion", stream: failedTool + "\n" + textEvent(valid), wantErr: true},
		{name: "stream error remains invalid", stream: failedTool + "\n" + textEvent(valid) + "\n" + completion + "\n{\"type\":\"error\"}", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := ParseOpenCodeEnvelope([]byte(tt.stream), true)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected invalid envelope, got %#v", parsed)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Response["result"] != NoMaterialFinding {
				t.Fatalf("response = %#v", parsed.Response)
			}
			if parsed.Usage["reviewer_failed_tool_calls"] != 1 {
				t.Fatalf("usage = %#v", parsed.Usage)
			}
		})
	}
}

func TestResultMappingsGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/results/mappings.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]map[string]any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	completed, err := Completed("CHANGES_REQUIRED", "FAIL", []Finding{{Code: "BUG-1", Message: "bad behavior"}}, "cx-3", map[string]any{"reviewer1_model_requested": "wanted/model"}, floatPtr(1.25), map[string]any{"input_tokens": 10}, "codex", "other/model-actual", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Completed(Reviewer1Unavailable, "UNKNOWN", nil, "", nil, nil, nil, "", "", 0); err == nil {
		t.Fatal("failure result was accepted as a completed review")
	}
	got := map[string]map[string]any{
		"completed":   completed.Map(),
		"unavailable": Unavailable("REVIEWER_RESPONSE_INVALID", "invalid response", map[string]any{"reviewer1_model_requested": "wanted/model"}, "codex").Map(),
		"incomplete":  IncompletePacket(PacketOmitted{Code: "PACKET_INCOMPLETE", Message: "missing context", Omitted: []string{"delegated_diff"}}, map[string]any{"reviewer1_model_requested": "wanted/model"}, "opencode").Map(),
	}
	for name, expected := range want {
		gotJSON, _ := json.Marshal(got[name])
		wantJSON, _ := json.Marshal(expected)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s = %s, want %s", name, gotJSON, wantJSON)
		}
	}
}

func TestAgyUsageSurvivesBridgeOutcome(t *testing.T) {
	parsed, err := ParseAgyEnvelope([]byte(`{"conversation_id":"agy-1","status":"SUCCESS","response":"ignored prose","duration_seconds":1.25,"num_turns":2,"usage":{"input_tokens":30000,"thinking_tokens":22000},"structured_output":{"result":"NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE","validation":"PASS","findings":[]}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := bridgeOutcome(CompletedReview{
		Result: NoMaterialFinding, Validation: "PASS", ConversationID: parsed.ConversationID,
		Usage: parsed.Usage, DurationSeconds: parsed.DurationSeconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := outcome.Attributes["usage"].(map[string]any)
	if !ok || usage["input_tokens"] != json.Number("30000") || usage["thinking_tokens"] != json.Number("22000") {
		t.Fatalf("bridge usage = %#v", outcome.Attributes["usage"])
	}
}

func TestStderrTail(t *testing.T) {
	if StderrTail(nil) != nil || StderrTail("") != nil {
		t.Fatal("empty stderr should be absent")
	}
	value := strings.Repeat("a", 2047) + "éx"
	got := StderrTail(value)
	if got == nil || *got != strings.Repeat("a", 2046)+"éx" {
		t.Fatalf("stderr tail = %v", got)
	}
}

func floatPtr(value float64) *float64 { return &value }
