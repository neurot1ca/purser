package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestComputeDeltaPythonFixture(t *testing.T) {
	file := filepath.Join("testdata", "compute_delta.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name    string   `json:"name"`
			Scope   []string `json:"scope"`
			Changes []struct {
				Op      string `json:"op"`
				Path    string `json:"path"`
				Content string `json:"content"`
			} `json:"changes"`
			Baseline map[string]any  `json:"baseline"`
			Result   json.RawMessage `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root := initGitRepository(t)
			for name, content := range map[string]string{"scope/tracked.txt": "base\n", "scope/delete.txt": "delete me\n", "outside/tracked.txt": "outside\n"} {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git(t, root, "add", "--all")
			t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00+00:00")
			t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00+00:00")
			git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
			for _, change := range test.Changes {
				path := filepath.Join(root, filepath.FromSlash(change.Path))
				if change.Op == "delete" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(change.Content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertBaselineExpected(t, ComputeDelta(root, test.Baseline, test.Scope), test.Result)
		})
	}
}
