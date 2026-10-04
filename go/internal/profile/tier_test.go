package profile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type tierFixtureCase struct {
	Input struct {
		Tier string         `json:"tier"`
		Data map[string]any `json:"data"`
	} `json:"input"`
	Output Tier `json:"output"`
}

func TestValidateTierRetainsWarningsAndFlattenedValues(t *testing.T) {
	got := ValidateTier("user", map[string]any{
		"schema_version": 1,
		"telemetry":      map[string]any{"vault_root": "/vault"},
		"worktree":       map[string]any{"copy": []any{".env"}, "setup_timeout_seconds": 9000},
	})
	if got.Status != "ok" || got.Values["telemetry.vault_root"] != "/vault" || len(got.Warnings) != 1 {
		t.Fatalf("got %#v, want retained tier with one worktree warning", got)
	}
}

func TestTierPythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_profile", "tier.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture []tierFixtureCase
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture {
		got := ValidateTier(test.Input.Tier, test.Input.Data)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(test.Output)
		var actual, want any
		_ = json.Unmarshal(gotJSON, &actual)
		_ = json.Unmarshal(wantJSON, &want)
		if !reflect.DeepEqual(actual, want) {
			t.Errorf("%s (%#v): got %s, want %s", test.Input.Tier, test.Input.Data, gotJSON, wantJSON)
		}
	}
}
