package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestWorkerValidationParityFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/worker_validation.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name      string         `json:"name"`
			Report    *string        `json:"report"`
			Canonical bool           `json:"canonical"`
			Summary   *string        `json:"summary"`
			Expected  map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			if item.Canonical {
				assertValidationJSON(t, canonicalUnknownValidation(item.Summary), item.Expected)
				return
			}
			report := ""
			if item.Report != nil {
				report = *item.Report
			}
			assertValidationJSON(t, parseWorkerValidation(report), item.Expected)
		})
	}
}
func assertValidationJSON(t *testing.T, got, want map[string]any) {
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("validation differs\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}
