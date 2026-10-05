package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"purser-hook/internal/profile"
	"reflect"
	"testing"
)

func TestAttributeKnownConcurrentScopesPythonFixture(t *testing.T) {
	file := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "attribution.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		EngineProtected []string `json:"engine_protected"`
		Cases           []struct {
			Name            string           `json:"name"`
			Delta           map[string]any   `json:"delta"`
			OwnScope        []string         `json:"own_scope"`
			Owners          []map[string]any `json:"owners"`
			ProtectedPaths  *[]string        `json:"protected_paths"`
			AdditionalPaths []string         `json:"additional_paths"`
			Protected       []string         `json:"protected"`
			Result          json.RawMessage  `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(profile.EngineProtectedPaths(), fixture.EngineProtected) {
		t.Fatalf("engine protected patterns differ from Python fixture: got %#v, want %#v", profile.EngineProtectedPaths(), fixture.EngineProtected)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			if got := DetectProtectedPaths(test.Delta, test.ProtectedPaths); !reflect.DeepEqual(got, test.Protected) {
				t.Fatalf("DetectProtectedPaths() = %#v, want %#v", got, test.Protected)
			}
			gotDelta, gotKnown := AttributeKnownConcurrentScopes(test.Delta, test.OwnScope, test.Owners, test.ProtectedPaths, test.AdditionalPaths)
			actual, err := json.Marshal(map[string]any{"delta": gotDelta, "known": gotKnown})
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(actual, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(test.Result, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("attribution mismatch\n got: %s\nwant: %s", actual, test.Result)
			}
		})
	}
}
