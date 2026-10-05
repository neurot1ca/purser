package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJobWorktreePythonFixture(t *testing.T) {
	file := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "job_worktree.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name    string         `json:"name"`
			Trusted bool           `json:"trusted"`
			Copy    string         `json:"copy"`
			Want    map[string]any `json:"want"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			base, err := filepath.Abs(gitTempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			repo, parent := filepath.Join(base, "repo"), filepath.Join(base, "jobs")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "init")
			git(t, repo, "config", "user.name", "fixture")
			git(t, repo, "config", "user.email", "fixture@example.invalid")
			git(t, repo, "config", "core.autocrlf", "false")
			writePatchTestFile(t, filepath.Join(repo, "tracked.txt"), []byte("base\n"))
			git(t, repo, "add", "tracked.txt")
			git(t, repo, "commit", "-m", "base")
			for name, body := range map[string]string{"tracked.txt": "dirty\n", "new.txt": "untracked\n", ".gitignore": "ignored.dat\n", "ignored.dat": "ignored\n", ".orchestrator/profile.toml": "schema_version = 1\n[worktree]\ncopy = [\"" + tc.Copy + "\"]\n"} {
				writePatchTestFile(t, filepath.Join(repo, filepath.FromSlash(name)), []byte(body))
			}
			home := filepath.Join(base, "home")
			user := filepath.Join(home, ".orchestrator", "config.toml")
			config := "schema_version = 1\n[trust]\npaths = [\"" + filepath.ToSlash(repo) + "\"]\n"
			if !tc.Trusted {
				config = "schema_version = 1\n[trust]\npaths = []\n"
			}
			writePatchTestFile(t, user, []byte(config))
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			got, createErr := CreateJobWorktree(repo, "job-worktree-fixture", parent)
			actual := map[string]any{"head": "", "snapshot": "", "files": map[string]string{}, "error": createErr != nil, "cleaned": false, "index_matches_snapshot": false}
			if createErr == nil {
				actual["head"], actual["snapshot"] = "@HEAD@", "@SNAPSHOT@"
				actual["files"] = worktreeFiles(t, got["path"])
				tree, err := runGit(got["path"], []string{"write-tree"}, time.Second, "checking job worktree index", nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				actual["index_matches_snapshot"] = strings.TrimSpace(string(tree)) == got["snapshot"]
				if len(got["head"]) < 40 || len(got["snapshot"]) < 40 {
					t.Fatalf("missing hashes: %#v", got)
				}
			}
			_, statErr := os.Stat(filepath.Join(parent, "job-worktree"))
			actual["cleaned"] = os.IsNotExist(statErr) && strings.Count(stringsOutput(t, repo, "worktree", "list", "--porcelain"), "worktree ") == 1
			want, _ := json.Marshal(tc.Want)
			actualJSON, _ := json.Marshal(actual)
			if !reflect.DeepEqual(actualJSON, want) {
				t.Fatalf("worktree = %s, want %s", actualJSON, want)
			}
		})
	}
}

func worktreeFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if !info.IsDir() && filepath.ToSlash(rel) != ".git" {
			b, e := os.ReadFile(p)
			if e == nil {
				files[filepath.ToSlash(rel)] = string(b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
