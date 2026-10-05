package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/ledger"
)

func TestApplyPythonOracle(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_apply", "oracle.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string         `json:"name"`
			Setup    map[string]any `json:"setup"`
			Snapshot string         `json:"snapshot"`
			Reviewed any            `json:"reviewedSnapshot"`
			Result   map[string]any `json:"result"`
			Record   map[string]any `json:"record"`
			Checkout string         `json:"checkout"`
			Index    map[string]any `json:"index"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			state := filepath.Join(base, "state")
			worktree := filepath.Join(base, "worktree")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "init", "-q")
			write(t, filepath.Join(repo, "tracked.txt"), "base\n")
			git(t, repo, "add", "--all")
			git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
			git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
			snapshot, err := ledger.CheckoutSnapshot(worktree)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != c.Snapshot {
				t.Fatalf("snapshot got %q want %q", snapshot, c.Snapshot)
			}
			write(t, filepath.Join(worktree, "tracked.txt"), c.Setup["worktree"].(string))
			write(t, filepath.Join(repo, "tracked.txt"), c.Setup["repo"].(string))
			jobID := sha256.Sum256([]byte(c.Name))
			id := hex.EncodeToString(jobID[:])
			job := map[string]any{"jobId": id, "state": c.Setup["state"], "repoRoot": repo, "worktreePath": worktree, "snapshot": snapshot}
			if c.Reviewed != nil {
				job["reviewedSnapshot"] = c.Reviewed
				job["snapshotRevision"] = float64(2)
			}
			jobDir := filepath.Join(state, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(jobDir, 0700); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(job)
			write(t, filepath.Join(jobDir, id+".json"), string(encoded))
			if c.Reviewed != nil {
				index := filepath.Join(state, "async_jobs", id+".json")
				if err := os.MkdirAll(filepath.Dir(index), 0700); err != nil {
					t.Fatal(err)
				}
				write(t, index, `{"stage":"APPLY"}`)
			}
			result := Apply(state, map[string]any{"jobId": id})
			if c.Setup["repeat"] == true {
				result = Apply(state, map[string]any{"jobId": id})
			}
			var got map[string]any
			rb, _ := json.Marshal(result["structuredContent"])
			_ = json.Unmarshal(rb, &got)
			if got["worktreePath"] != nil {
				got["worktreePath"] = "WORKTREE"
			}
			if !reflect.DeepEqual(got, c.Result) {
				t.Errorf("result got %#v want %#v", got, c.Result)
			}
			stored, err := os.ReadFile(filepath.Join(jobDir, id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(stored, &record); err != nil {
				t.Fatal(err)
			}
			delete(record, "appliedAt")
			record["repoRoot"] = "REPO"
			record["worktreePath"] = "WORKTREE"
			if apply, ok := record["applyResult"].(map[string]any); ok && apply["worktreePath"] != nil {
				apply["worktreePath"] = "WORKTREE"
			}
			if !reflect.DeepEqual(record, c.Record) {
				t.Errorf("record got %#v want %#v", record, c.Record)
			}
			checkout, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.ReplaceAll(string(checkout), "\r\n", "\n") != c.Checkout {
				t.Errorf("checkout got %q want %q", checkout, c.Checkout)
			}
			if c.Index != nil {
				index, err := os.ReadFile(filepath.Join(state, "async_jobs", id+".json"))
				if err != nil {
					t.Fatal(err)
				}
				var gotIndex map[string]any
				_ = json.Unmarshal(index, &gotIndex)
				if !reflect.DeepEqual(gotIndex, c.Index) {
					t.Errorf("index got %#v want %#v", gotIndex, c.Index)
				}
			}
		})
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE=2000-01-01T00:00:00+00:00")
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
}
func write(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
