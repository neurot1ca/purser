package profile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type entriesFixtureCase struct {
	Input struct {
		Operation string `json:"operation"`
		Raw       any    `json:"raw"`
		Tier      string `json:"tier"`
		Value     any    `json:"value"`
	} `json:"input"`
	Output map[string]any `json:"output"`
}

func TestParseChecksAcceptsValidCheck(t *testing.T) {
	checks, reason := ParseChecks([]any{map[string]any{"name": "lint", "argv": []any{"go", "vet", "./..."}}})
	if reason != "" || len(checks) != 1 || checks[0].TimeoutSeconds != 60 {
		t.Fatalf("got (%#v, %q), want one defaulted check", checks, reason)
	}
}

func TestEntriesPythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_profile", "entries.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture []entriesFixtureCase
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture {
		var got any
		switch test.Input.Operation {
		case "checks":
			checks, reason := ParseChecks(test.Input.Raw)
			got = map[string]any{"checks": checks, "error": reason}
		case "authority":
			sources, warnings := ParseAuthoritySources(test.Input.Tier, test.Input.Raw)
			got = map[string]any{"sources": sources, "warnings": warnings}
		case "path":
			path, valid := AuthorityPath(test.Input.Value)
			var normalized any
			if valid {
				normalized = path
			}
			got = map[string]any{"path": normalized, "valid": valid}
		case "authority_defaults":
			got = map[string]any{"sources": DefaultAuthoritySources()}
		default:
			t.Fatalf("unknown fixture operation %q", test.Input.Operation)
		}
		wantJSON, _ := json.Marshal(test.Output)
		gotJSON, _ := json.Marshal(got)
		var want, actual any
		_ = json.Unmarshal(wantJSON, &want)
		_ = json.Unmarshal(gotJSON, &actual)
		if !reflect.DeepEqual(actual, want) {
			t.Errorf("%s (%#v): got %s, want %s", test.Input.Operation, test.Input, gotJSON, wantJSON)
		}
	}
}
