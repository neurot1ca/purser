package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDriftAndApplyJobPatchPythonFixture(t *testing.T) {
	file := filepath.Join("testdata", "apply_patch.json")
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

func TestApplyJobPatchConflictDoesNotWriteToWorkingDirectory(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	initReplayRepository(t, repo, "base\n")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("checkout\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workdir := filepath.Join(base, "workdir")
	if err := os.Mkdir(workdir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workdir)

	patch := "diff --git a/tracked.txt b/tracked.txt\nindex df967b96a579e45a18b8251732d16804b2e56a55..ff0d03824e95bfade49b6601975b1a04bf24dc66 100644\n--- a/tracked.txt\n+++ b/tracked.txt\n@@ -1 +1 @@\n-base\n+worker\n"
	result := ApplyJobPatch(repo, patch)
	if result["applied"] != false {
		t.Fatalf("patch result = %#v, want a conflict", result)
	}
	conflicts, ok := result["conflicts"].([]string)
	if !ok || !reflect.DeepEqual(conflicts, []string{"tracked.txt"}) {
		t.Fatalf("patch conflicts = %#v, want tracked.txt", result["conflicts"])
	}
	if _, err := os.Stat(filepath.Join(workdir, "tracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("conflict file in working directory: stat err = %v", err)
	}
}
