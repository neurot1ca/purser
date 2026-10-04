package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPythonConfigGoldenParity(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "config_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string         `json:"name"`
		Input  map[string]any `json:"input"`
		OK     bool           `json:"ok"`
		Error  string         `json:"error"`
		Loaded map[string]any `json:"loaded"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			err := ValidateConfig(tc.Input)
			if (err == nil) != tc.OK {
				t.Fatalf("ValidateConfig error = %v, want ok=%v", err, tc.OK)
			}
			if err != nil && err.Error() != tc.Error {
				t.Fatalf("ValidateConfig error = %q, want %q", err, tc.Error)
			}
			loaded, err := LoadConfig(tc.Input)
			if !tc.OK {
				if err == nil || err.Error() != tc.Error {
					t.Fatalf("LoadConfig error = %v, want %q", err, tc.Error)
				}
				return
			}
			if err != nil || !jsonEqual(loaded, tc.Loaded) {
				t.Fatalf("LoadConfig = %#v, %v; want %#v", loaded, err, tc.Loaded)
			}
		})
	}
}

func jsonEqual(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}
