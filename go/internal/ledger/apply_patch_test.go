package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDriftAndApplyJobPatchPythonFixture(t *testing.T) {
	file := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_ledger_apply", "oracle.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Checkout, Patch string
			Paths, Drift          []string
			Outcome               map[string]any
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			initReplayRepository(t, repo, "base\n")
			snapshot, err := CheckoutSnapshot(repo)
			if err != nil {
				t.Fatal(err)
			}
			if c.Name != "unsafe_path" {
				if err = os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte(c.Checkout), 0600); err != nil {
					t.Fatal(err)
				}
			}
			drift, err := SnapshotDriftPaths(repo, snapshot, c.Paths)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(drift, c.Drift) {
				t.Fatalf("drift = %#v, want %#v", drift, c.Drift)
			}
			got := ApplyJobPatch(repo, c.Patch)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(c.Outcome)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("outcome = %#v, want %#v", got, c.Outcome)
			}
		})
	}
}
