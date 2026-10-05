package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPostRepairDeltaPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_repair_delta.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Case        map[string]any `json:"case"`
			Baseline    map[string]any `json:"baseline"`
			Scope       []string       `json:"scope"`
			RepairDelta map[string]any `json:"repair_delta"`
			Expected    map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		name, _ := tc.Case["name"].(string)
		t.Run(name, func(t *testing.T) {
			root := postDeltaRepo(t)
			initial := filepath.Join(root, "scope", "file.txt")
			if err := os.WriteFile(initial, []byte(strings.Repeat("initial\n", 60)), 0600); err != nil {
				t.Fatal(err)
			}
			repair, _ := tc.Case["repair"].([]any)
			for _, raw := range repair {
				change, _ := raw.(map[string]any)
				path := filepath.Join(root, filepath.FromSlash(change["path"].(string)))
				if err := os.WriteFile(path, []byte(change["content"].(string)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pending := map[string]any{"phase": "implementation", "scope": tc.Scope,
				"baseline": tc.Baseline, "lease_owner": "fixture-owner"}
			bundle := baseBundle("")
			bundle["force_full_review"] = false
			protected, _ := tc.Case["protected"].([]any)
			paths := make([]string, 0, len(protected))
			for _, path := range protected {
				paths = append(paths, path.(string))
			}
			if tc.Case["delta_failure"] == true {
				// A failed recomputation replaces delta and findings, while preserving
				// the path and scope evidence collected before repair.
				bundle["changed_paths"] = []string{"pre-repair.txt"}
				bundle["whole_tree_anomaly"] = map[string]any{"paths": []string{"pre-repair.txt"}}
				tc.Baseline = map[string]any{"status": "UNKNOWN", "code": "DELTA_INSPECTION_FAILED",
					"message": "fixture delta failure"}
				pending["baseline"] = map[string]any{"status": "UNKNOWN", "code": "DELTA_INSPECTION_FAILED", "message": "fixture delta failure"}
				delta := recomputePostRepairDelta(root, pending, bundle, paths, []map[string]any{}, true)
				if !reflect.DeepEqual(bundle["changed_paths"], []string{"pre-repair.txt"}) ||
					!reflect.DeepEqual(bundle["whole_tree_anomaly"], map[string]any{"paths": []string{"pre-repair.txt"}}) {
					t.Fatalf("failed delta replaced pre-repair evidence: %#v", bundle)
				}
				if got := delta["code"]; got != "DELTA_INSPECTION_FAILED" {
					t.Fatalf("failed delta code = %v", got)
				}
				if bundle["force_full_review"] != true {
					t.Fatal("failed delta must force FULL review")
				}
				return
			}
			delta := recomputePostRepairDelta(root, pending, bundle, paths, []map[string]any{}, true)
			for _, key := range []string{"whole_tree_delta", "whole_tree_anomaly", "known_concurrent_scopes",
				"changed_paths", "delta_lines", "delta_files", "current_head", "protected_paths_touched", "concurrent_writer"} {
				want := tc.Expected[key]
				gotJSON, _ := json.Marshal(bundle[key])
				wantJSON, _ := json.Marshal(want)
				if !reflect.DeepEqual(gotJSON, wantJSON) {
					t.Errorf("%s mismatch\n got: %s\nwant: %s", key, gotJSON, wantJSON)
				}
			}
			if delta["status"] != tc.Expected["delta_status"] {
				t.Errorf("delta status = %v, want %v", delta["status"], tc.Expected["delta_status"])
			}
			if name == "repair_out_of_scope" {
				findings, _ := bundle["findings"].([]any)
				found := false
				for _, finding := range findings {
					if finding.(map[string]any)["code"] == "OUT_OF_SCOPE_MODIFICATION" {
						found = true
					}
				}
				if !found {
					t.Fatalf("out-of-scope finding = %#v", findings)
				}
			}
			if name == "repair_protected_path" {
				protected, _ := bundle["protected_paths_touched"].([]string)
				if len(protected) != 1 || protected[0] != "secrets/key.txt" {
					t.Fatalf("protected paths = %#v", bundle["protected_paths_touched"])
				}
			}
		})
	}
}
