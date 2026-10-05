package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOutOfScopePythonFixture(t *testing.T) {
	file := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "out_of_scope.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name                string          `json:"name"`
			Scope               []string        `json:"scope"`
			PreChanges          []fixtureChange `json:"pre_changes"`
			StageBeforeBaseline []string        `json:"stage_before_baseline"`
			Changes             []fixtureChange `json:"changes"`
			CommitChanges       bool            `json:"commit_changes"`
			Baseline            map[string]any  `json:"baseline"`
			Result              json.RawMessage `json:"result"`
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root := initGitRepository(t)
			for name, content := range map[string]string{"scope/tracked.txt": "base\n", "outside/tracked.txt": "outside\n"} {
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
			for _, change := range test.PreChanges {
				writeFixtureChange(t, root, change)
			}
			if len(test.StageBeforeBaseline) > 0 {
				git(t, root, append([]string{"add"}, test.StageBeforeBaseline...)...)
			}
			for _, change := range test.Changes {
				writeFixtureChange(t, root, change)
			}
			if test.CommitChanges {
				git(t, root, "add", "--all")
				git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "after baseline")
			}
			assertBaselineExpected(t, DetectOutOfScopeModifications(root, test.Baseline, test.Scope), test.Result)
		})
	}
}

type fixtureChange struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func writeFixtureChange(t *testing.T, root string, change fixtureChange) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(change.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(change.Content), 0600); err != nil {
		t.Fatal(err)
	}
}
