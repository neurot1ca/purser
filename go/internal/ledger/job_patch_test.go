package ledger

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJobReplayPythonFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "job_patch.json"))
	checkPatchError(t, err)
	var fixture struct {
		Cases []struct {
			Name, Patch string
			Changes     []map[string]string `json:"changes"`
		} `json:"patch_cases"`
	}
	checkPatchError(t, json.Unmarshal(data, &fixture))
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root, snapshot := initJobPatchRepository(t)
			for _, change := range test.Changes {
				path := filepath.Join(root, filepath.FromSlash(change["path"]))
				if change["op"] == "delete" {
					checkPatchError(t, os.Remove(path))
					continue
				}
				content := []byte(change["content"])
				if change["content_hex"] != "" {
					content, err = hex.DecodeString(change["content_hex"])
					checkPatchError(t, err)
				}
				writePatchTestFile(t, path, content)
			}
			got, err := JobPatch(root, snapshot)
			checkPatchError(t, err)
			if got != test.Patch {
				t.Fatalf("patch mismatch\n got: %q\nwant: %q", got, test.Patch)
			}
			if test.Name == "empty" {
				clean := replaySnapshot(t, root)
				writePatchTestFile(t, filepath.Join(root, "tracked.txt"), []byte("dirty\n"))
				writePatchTestFile(t, filepath.Join(root, "new.txt"), []byte("new\n"))
				dirty := replaySnapshot(t, root)
				if clean == dirty || dirty != replaySnapshot(t, root) {
					t.Fatal("checkout snapshot mismatch or instability")
				}
			}
		})
	}
}

func initJobPatchRepository(t *testing.T) (string, string) {
	root := initGitRepository(t)
	for name, content := range map[string][]byte{"tracked.txt": []byte("base\n"), "delete.txt": []byte("delete\n"), "binary.dat": {0, 1, 2, 255}} {
		writePatchTestFile(t, filepath.Join(root, name), content)
	}
	git(t, root, "add", "--all")
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	return root, stringsOutput(t, root, "rev-parse", "HEAD^{tree}")
}

func writePatchTestFile(t *testing.T, path string, content []byte) {
	t.Helper()
	checkPatchError(t, os.MkdirAll(filepath.Dir(path), 0700))
	checkPatchError(t, os.WriteFile(path, content, 0600))
}

func checkPatchError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func replaySnapshot(t *testing.T, root string) string {
	t.Helper()
	snapshot, err := CheckoutSnapshot(root)
	checkPatchError(t, err)
	return snapshot
}

func TestFilterJobDeltaFatalGitFailureKeepsDelta(t *testing.T) {
	root := gitTempDir(t)
	checkPatchError(t, os.WriteFile(filepath.Join(root, ".git"), []byte("invalid"), 0600))
	delta := map[string]any{"status": "OK", "changed_paths": []string{"tracked.txt"}, "changed_file_count": 1}
	got := FilterJobDelta(root, delta)
	if got["changed_file_count"] != 1 {
		t.Fatal(got)
	}
}
