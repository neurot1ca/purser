package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPostProfileMatchesPythonOracle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profiles []map[string]any `json:"profiles"`
		Out      []map[string]any `json:"out_of_scope"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Profiles {
		t.Run(test["name"].(string), func(t *testing.T) {
			got, _ := json.Marshal(postProfileFromPending(test["pending"].(map[string]any), test["config"].(map[string]any)))
			var actual map[string]any
			_ = json.Unmarshal(got, &actual)
			if !reflect.DeepEqual(actual, test["expected"]) {
				t.Fatalf("post profile = %#v, want Python oracle %#v", actual, test["expected"])
			}
		})
	}
	for _, test := range fixture.Out {
		t.Run(test["name"].(string), func(t *testing.T) {
			bundle := test["bundle"].(map[string]any)
			paths, _ := postStringList(test["protected_paths"])
			forced := applyOutOfScopeResult(bundle, test["result"].(map[string]any), paths)
			if forced != test["force_full"].(bool) {
				t.Fatalf("force full = %v, want %v", forced, test["force_full"])
			}
			got, _ := json.Marshal(bundle)
			var actual map[string]any
			_ = json.Unmarshal(got, &actual)
			if !reflect.DeepEqual(actual, test["expected_bundle"]) {
				t.Fatalf("bundle = %#v, want Python oracle %#v", actual, test["expected_bundle"])
			}
		})
	}
}
