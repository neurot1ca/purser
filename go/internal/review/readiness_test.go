package review

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadinessVerdictOracleGoldens(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "readiness", "verdicts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Criteria map[string]any `json:"criteria"`
		Diff     string         `json:"diff"`
		Cases    []struct {
			Name     string         `json:"name"`
			Payload  map[string]any `json:"payload"`
			Grounded bool           `json:"grounded"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			criteria := map[string]any{"grounded": tc.Grounded, "acceptance_criteria": fixture.Criteria["acceptance_criteria"]}
			encoded, _ := json.Marshal(tc.Payload)
			got, _ := json.Marshal(ParseReadinessVerdict(string(encoded), criteria, fixture.Diff))
			want, _ := json.Marshal(tc.Expected)
			if !bytes.Equal(got, want) {
				t.Fatalf("verdict = %s, want %s", got, want)
			}
		})
	}
}
