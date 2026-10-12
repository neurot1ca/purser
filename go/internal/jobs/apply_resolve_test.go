package jobs

import "purser-hook/internal/testutil"

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/ledger"
)

func TestApplyResolvedPythonOracle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_resolve", "oracle.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name        string         `json:"name"`
			Setup       map[string]any `json:"setup"`
			JobID       string         `json:"jobId"`
			Sibling     string         `json:"siblingJobId"`
			Snapshot    string         `json:"snapshot"`
			Result      map[string]any `json:"result"`
			Record      map[string]any `json:"record"`
			Checkout    string         `json:"checkout"`
			SideEffects []any          `json:"sideEffects"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			base := t.TempDir()
			repo, state, worktree := filepath.Join(base, "repo"), filepath.Join(base, "state"), filepath.Join(base, "worktree")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "init", "-q")
			write(t, filepath.Join(repo, "tracked.txt"), "base\n")
			git(t, repo, "add", "--all")
			git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
			branch := strings.TrimSpace(string(gitOutput(t, repo, "branch", "--show-current")))
			git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
			snapshot, err := ledger.CheckoutSnapshot(worktree)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != c.Snapshot {
				t.Fatalf("snapshot got %q want %q", snapshot, c.Snapshot)
			}
			write(t, filepath.Join(worktree, "tracked.txt"), "worker change\n")
			if c.Setup["merge"] == true {
				git(t, repo, "checkout", "-b", "side")
				write(t, filepath.Join(repo, "tracked.txt"), "side change\n")
				git(t, repo, "add", "tracked.txt")
				git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "side")
				git(t, repo, "checkout", branch)
				write(t, filepath.Join(repo, "tracked.txt"), "main change\n")
				git(t, repo, "add", "tracked.txt")
				git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "main")
				cmd := testutil.Hide(exec.Command("git", "merge", "side"))
				cmd.Dir = repo
				cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
				_ = cmd.Run() // A conflict is the expected nonzero result.
				unmerged := testutil.Hide(exec.Command("git", "diff", "--name-only", "--diff-filter=U", "-z"))
				unmerged.Dir = repo
				if output, err := unmerged.Output(); err != nil || len(output) == 0 {
					t.Fatalf("merge fixture did not create unmerged paths: %q, %v", output, err)
				}
			} else if replacement, ok := c.Setup["repo"].(string); ok {
				write(t, filepath.Join(repo, "tracked.txt"), replacement)
			}
			job := map[string]any{"jobId": c.JobID, "state": "SUCCEEDED", "repoRoot": repo, "worktreePath": worktree, "snapshot": snapshot, "taskId": "fixture-task"}
			if c.Setup["outcome"] != "not_in_conflict" {
				job["applyState"] = "conflict"
			} else {
				job["applyState"] = nil
			}
			jobDir := filepath.Join(state, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(jobDir, 0700); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(job)
			write(t, filepath.Join(jobDir, c.JobID+".json"), string(encoded))
			if c.Sibling != "" {
				sibling := map[string]any{"jobId": c.Sibling, "state": "SUCCEEDED", "repoRoot": repo, "worktreePath": worktree, "snapshot": snapshot}
				raw, _ := json.Marshal(sibling)
				write(t, filepath.Join(jobDir, c.Sibling+".json"), string(raw))
			}
			effects := []any{}
			options := ApplyOptions{
				RecordScopeConflictOutcome: func(job map[string]any, action string) {
					effects = append(effects, map[string]any{"call": "scope", "jobId": job["jobId"], "action": action})
				},
				RecordDecision: func(job map[string]any, decision, kind, source string) {
					effects = append(effects, map[string]any{"call": "decision", "jobId": job["jobId"], "decision": decision, "kind": kind, "sourceJobId": source})
				},
				ReleaseScopeLease: func(_ string, id string) bool {
					effects = append(effects, map[string]any{"call": "release", "jobId": id})
					return true
				},
			}
			gotRaw, _ := json.Marshal(Apply(state, map[string]any{"jobId": c.JobID, "resolved": true}, options)["structuredContent"])
			var got map[string]any
			_ = json.Unmarshal(gotRaw, &got)
			delete(got, "validation")
			if !reflect.DeepEqual(got, c.Result) {
				t.Errorf("result got %#v want %#v", got, c.Result)
			}
			stored, err := os.ReadFile(filepath.Join(jobDir, c.JobID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(stored, &record); err != nil {
				t.Fatal(err)
			}
			delete(record, "appliedAt")
			record["repoRoot"], record["worktreePath"] = "REPO", "WORKTREE"
			if apply, ok := record["applyResult"].(map[string]any); ok && apply["worktreePath"] != nil {
				apply["worktreePath"] = "WORKTREE"
			}
			if !reflect.DeepEqual(record, c.Record) {
				t.Errorf("record got %#v want %#v", record, c.Record)
			}
			if !reflect.DeepEqual(effects, c.SideEffects) {
				t.Errorf("side effects got %#v want %#v", effects, c.SideEffects)
			}
			checkout, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.ReplaceAll(string(checkout), "\r\n", "\n") != c.Checkout {
				t.Errorf("checkout got %q want %q", checkout, c.Checkout)
			}
		})
	}
}

func TestResolvedJobPathAllowsSymlinkedTempAncestor(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	repo := filepath.Join(real, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	if _, ok := resolvedJobPath(filepath.Join(alias, "repo"), "tracked.txt"); !ok {
		t.Fatal("repository under a symlinked temporary ancestor was rejected")
	}
}

func gitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := testutil.Hide(exec.Command("git", args...))
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return b
}
