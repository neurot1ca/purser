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
	file := filepath.Join("testdata", "attribution.json")
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

func TestAttributeKnownConcurrentScopesMalformedOwnerFailsOpen(t *testing.T) {
	delta := map[string]any{"status": OK, "changed_paths": []string{"shared/file.txt"}, "changed_file_count": 1}
	cases := map[string][]map[string]any{
		"missing_task_id": {
			{"task_id": "task-a", "scope": []string{"shared"}},
			{"scope": []string{"shared"}},
		},
		"malformed_scope": {
			{"task_id": "task-a", "scope": []string{"shared"}},
			{"task_id": "task-b", "scope": []any{"shared", 1}},
		},
	}
	for name, owners := range cases {
		t.Run(name, func(t *testing.T) {
			gotDelta, known := AttributeKnownConcurrentScopes(delta, nil, owners, nil, nil)
			if len(known) != 0 || !reflect.DeepEqual(gotDelta, delta) {
				t.Fatalf("malformed owner must preserve delta without attribution: delta=%#v known=%#v", gotDelta, known)
			}
		})
	}
}

func TestClassifyConcurrentWritersPythonFixture(t *testing.T) {
	file := filepath.Join("testdata", "concurrent_writers.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name              string          `json:"name"`
			Delta             map[string]any  `json:"delta"`
			Scope             []string        `json:"scope"`
			MaterialPaths     []string        `json:"material_paths"`
			VerificationPaths []string        `json:"verification_paths"`
			Result            json.RawMessage `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			gotBytes, err := json.Marshal(ClassifyConcurrentWriters(test.Delta, test.Scope, test.MaterialPaths, test.VerificationPaths, nil))
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(gotBytes, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(test.Result, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("classification mismatch\n got: %s\nwant: %s", gotBytes, test.Result)
			}
		})
	}
}
