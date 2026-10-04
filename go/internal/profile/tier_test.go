package profile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type tierFixtureCase struct {
	Input struct {
		Tier string         `json:"tier"`
		TOML string         `json:"toml"`
		Data map[string]any `json:"data"`
	} `json:"input"`
	Output        Tier   `json:"output"`
	WarningPrefix string `json:"warning_prefix"`
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
	rawCases := []struct {
		tier string
		toml string
	}{
		{"user", `schema_version = 1
[telemetry]
vault_root = "/vault"
`},
		{"project", `schema_version = 1
[unknown]
"quoted.key" = "quoted"
dotted.key = "dotted"
multiline = """first line
second line"""
mixed = [1, 1.5, true]
inline = { integer = 2, float = 2.5, boolean = false }
[[unknown.rows]]
name = "row"
[project]
id = 7
[authority]
sources = ["bad"]
[worktree]
setup_timeout_seconds = 3601
`},
		{"user", `schema_version = 1
[review]
skip_max_lines = -1
`},
		{"user", "schema_version = 2\n"},
		{"user", "schema_version = 1\nchecks = []\n"},
		{"project", `schema_version = 1
[[checks]]
name = "lint"
argv = ["go", "vet", "./..."]
[worktree]
copy = [".env", "config.toml"]
setup = "go test ./..."
setup_timeout_seconds = 30
`},
		{"user", `schema_version = 1
[z_unknown]
[a_unknown]
[review]
z_invalid = true
a_invalid = true
`},
		{"project", `schema_version = 1
[worktree]
setup_timeout_seconds = 3601
copy = [1]
setup = ""
`},
		{"project", "schema_version = 1\n[[checks]]\nname = \"broken\"\n"},
		{"user", "schema_version = 1\nbroken = [\n"},
		{"user", "schema_version = 1\nfoo =\n"},
	}
	if len(rawCases) != len(fixture) {
		t.Fatalf("got %d raw cases, want %d golden cases", len(rawCases), len(fixture))
	}
	for i, test := range fixture {
		raw := []byte(rawCases[i].toml)
		data, keys, err := DecodeTOML(raw)
		if test.WarningPrefix != "" {
			got := ValidateTierTOML(rawCases[i].tier, raw)
			if err == nil || got.Status != test.Output.Status || !reflect.DeepEqual(got.Values, test.Output.Values) ||
				len(got.Warnings) != 1 || !strings.HasPrefix(got.Warnings[0], test.WarningPrefix) ||
				strings.TrimPrefix(got.Warnings[0], test.WarningPrefix) == "" {
				t.Errorf("case %d malformed TOML rejection shape differs: %#v (decode error %v)", i, got, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("case %d TOML: %v", i, err)
		}
		got := ValidateTier(rawCases[i].tier, data, keys)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(test.Output)
		var actual, want any
		_ = json.Unmarshal(gotJSON, &actual)
		_ = json.Unmarshal(wantJSON, &want)
		if !reflect.DeepEqual(actual, want) {
			t.Errorf("case %d (%s): got %s, want %s", i, rawCases[i].tier, gotJSON, wantJSON)
		}
		if i == 1 {
			unknown := data["unknown"].(map[string]any)
			if unknown["quoted.key"] != "quoted" || unknown["dotted"].(map[string]any)["key"] != "dotted" ||
				unknown["multiline"] != "first line\nsecond line" {
				t.Errorf("quoted, dotted, or multiline TOML values decoded incorrectly: %#v", unknown)
			}
			mixed := unknown["mixed"].([]any)
			inline := unknown["inline"].(map[string]any)
			if _, ok := mixed[0].(int64); !ok {
				t.Errorf("integer decoded as %T", mixed[0])
			}
			if _, ok := mixed[1].(float64); !ok {
				t.Errorf("float decoded as %T", mixed[1])
			}
			if _, ok := mixed[2].(bool); !ok {
				t.Errorf("boolean decoded as %T", mixed[2])
			}
			if _, ok := inline["integer"].(int64); !ok {
				t.Errorf("inline integer decoded as %T", inline["integer"])
			}
			if rows := unknown["rows"].([]any); len(rows) != 1 || rows[0].(map[string]any)["name"] != "row" {
				t.Errorf("table array decoded incorrectly: %#v", unknown["rows"])
			}
		}
	}
}
