package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPythonOrchGoldenParity(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "orch.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string `json:"name"`
		Input struct {
			Prompt any            `json:"prompt"`
			Config map[string]any `json:"config"`
		} `json:"input"`
		ExpectedCode    string         `json:"expected_code"`
		ExpectedMessage string         `json:"expected_message"`
		OK              bool           `json:"ok"`
		Result          map[string]any `json:"result"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := ParseOrchPrompt(tc.Input.Prompt, tc.Input.Config)
			gotJSON, _ := json.Marshal(got)
			var gotFields map[string]any
			if err := json.Unmarshal(gotJSON, &gotFields); err != nil {
				t.Fatal(err)
			}
			if got.Valid != tc.OK || !jsonEqual(gotFields, tc.Result) {
				wantJSON, _ := json.Marshal(tc.Result)
				t.Fatalf("ParseOrchPrompt = %s, want %s", gotJSON, wantJSON)
			}
			if !tc.OK && (got.Code != tc.ExpectedCode || got.Message != tc.ExpectedMessage) {
				t.Fatalf("rejected on (%q, %q), want branch (%q, %q)", got.Code, got.Message, tc.ExpectedCode, tc.ExpectedMessage)
			}
		})
	}
}
