package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareJobReplayStagesOnlyJobPaths(t *testing.T) {
	base := t.TempDir()
	repo, worker := filepath.Join(base, "repo"), filepath.Join(base, "worker")
	snapshot := initReplayRepository(t, repo, "")
	git(t, repo, "worktree", "add", "--detach", worker, "HEAD")
	writePatchTestFile(t, filepath.Join(worker, "tracked.txt"), []byte("worker\n"))
	writePatchTestFile(t, filepath.Join(repo, "other.txt"), []byte("outside scope\n"))
	var addArgs [][]string
	runner := func(root string, args []string, timeout time.Duration, operation string, input []byte, env map[string]string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "add" && args[1] == "-A" {
			addArgs = append(addArgs, append([]string(nil), args...))
		}
		return runGit(root, args, timeout, operation, input, env)
	}
	result, err := prepareJobReplay(repo, worker, snapshot, filepath.Join(base, "recovery"), nil, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "prepared" {
		t.Fatalf("unexpected replay result: %#v", result)
	}
	if len(addArgs) < 2 {
		t.Fatalf("expected patch and snapshot staging calls, got %#v", addArgs)
	}
	for _, args := range addArgs {
		if len(args) < 4 || args[2] != "--" || strings.Join(args[3:], " ") != ":(literal)tracked.txt" {
			t.Fatalf("replay preparation used unexpected add arguments: %#v", addArgs)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(worker, "other.txt")); string(got) != "other\n" {
		t.Fatalf("outside-scope checkout content leaked into replay snapshot: %q", got)
	}
}

func TestPrepareJobReplayRetryFromAppliedPreparingState(t *testing.T) {
	base := t.TempDir()
	repo, worker := filepath.Join(base, "repo"), filepath.Join(base, "worker")
	snapshot := initReplayRepository(t, repo, "")
	git(t, repo, "worktree", "add", "--detach", worker, "HEAD")
	if err := os.WriteFile(filepath.Join(worker, "tracked.txt"), []byte("job result\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch, err := JobPatch(worker, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	recovery := filepath.Join(base, "recovery")
	if err = os.Mkdir(recovery, 0700); err != nil {
		t.Fatal(err)
	}
	if err = copyReplayTree(worker, filepath.Join(recovery, "worker-tree")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(recovery, "worker.patch"), []byte(patch), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{"state": "PREPARING", "snapshot": snapshot, "target": snapshot, "paths": []string{"tracked.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(recovery, "state.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	restored := false
	runner := func(root string, args []string, timeout time.Duration, operation string, input []byte, env map[string]string) ([]byte, error) {
		if operation == "restoring worker index" {
			restored = true
		}
		return runGit(root, args, timeout, operation, input, env)
	}
	result, err := prepareJobReplay(repo, worker, snapshot, recovery, nil, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "prepared" {
		t.Fatalf("retry did not prepare: %#v", result)
	}
	if !restored {
		t.Fatal("retry did not restore the saved worker tree")
	}
	if got, err := os.ReadFile(filepath.Join(worker, "tracked.txt")); err != nil || string(got) != "job result\n" {
		t.Fatalf("retry produced wrong worker tree: %q, %v", got, err)
	}
}

func TestPrepareJobReplayThreeWayStagesAddedFile(t *testing.T) {
	base := t.TempDir()
	repo, worker := filepath.Join(base, "repo"), filepath.Join(base, "worker")
	snapshot := initReplayRepository(t, repo, "")
	git(t, repo, "worktree", "add", "--detach", worker, "HEAD")
	if err := os.WriteFile(filepath.Join(worker, "new.txt"), []byte("job added\n"), 0600); err != nil {
		t.Fatal(err)
	}
	patch, err := JobPatch(worker, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := []string{}
	merged, err := tryThreeWayJobPatch(repo, "", []string{"new.txt"}, []byte(patch), &conflicts)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) == 0 || len(conflicts) != 0 {
		t.Fatalf("three-way merge did not produce the added file patch: merged=%q conflicts=%v", merged, conflicts)
	}
}

func TestPrepareJobReplayWhenTargetAlreadyContainsNewOwnershipLock(t *testing.T) {
	base := t.TempDir()
	repo, worker := filepath.Join(base, "repo"), filepath.Join(base, "worker")
	snapshot := initReplayRepository(t, repo, "")
	git(t, repo, "worktree", "add", "--detach", worker, "HEAD")
	lockPath := filepath.Join("worktree_ownership", strings.Repeat("a", 64)+".owner-lock")
	writePatchTestFile(t, filepath.Join(repo, lockPath), nil)
	git(t, repo, "add", "--", lockPath)
	writePatchTestFile(t, filepath.Join(worker, lockPath), nil)
	directCheckFailed := false
	runner := func(root string, args []string, timeout time.Duration, operation string, input []byte, env map[string]string) ([]byte, error) {
		out, runErr := runGit(root, args, timeout, operation, input, env)
		if operation == "checking direct replay" && runErr != nil {
			directCheckFailed = true
		}
		return out, runErr
	}
	result, err := prepareJobReplay(repo, worker, snapshot, filepath.Join(base, "recovery"), nil, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !directCheckFailed || result["status"] != "prepared" {
		t.Fatalf("direct-check fallback with target ownership lock failed: directCheckFailed=%v result=%#v", directCheckFailed, result)
	}
}

func TestPrepareJobReplayPythonFixture(t *testing.T) {
	file := filepath.Join("testdata", "replay.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name          string              `json:"name"`
			Seed          string              `json:"seed"`
			BaseTracked   string              `json:"base_tracked"`
			Resolver      string              `json:"resolver"`
			Script        string              `json:"script"`
			TargetChanges []map[string]string `json:"target_changes"`
			WorkerChanges []map[string]string `json:"worker_changes"`
			Result        map[string]any      `json:"result"`
			Recovery      map[string]any      `json:"recovery"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			base := t.TempDir()
			repo, worker := filepath.Join(base, "repo"), filepath.Join(base, "worker")
			snapshot := initReplayRepository(t, repo, c.BaseTracked)
			git(t, repo, "worktree", "add", "--detach", worker, "HEAD")
			applyReplayChanges(t, repo, c.TargetChanges)
			applyReplayChanges(t, worker, c.WorkerChanges)
			jobPaths := make([]string, 0, len(c.WorkerChanges))
			for _, change := range c.WorkerChanges {
				jobPaths = append(jobPaths, change["path"])
			}
			target, err := checkoutSnapshotWithGit(repo, jobPaths, runGit)
			if err != nil {
				t.Fatal(err)
			}
			recovery := filepath.Join(base, "recovery")
			if c.Seed != "" {
				if err = os.Mkdir(recovery, 0700); err != nil {
					t.Fatal(err)
				}
				if c.Seed == "UNREADABLE" {
					err = os.WriteFile(filepath.Join(recovery, "state.json"), []byte("{bad json"), 0600)
				} else {
					state := map[string]any{"state": c.Seed, "snapshot": snapshot, "target": target, "paths": []string{"tracked.txt"}, "conflicts": []string{"tracked.txt"}}
					if c.Seed == "PREPARED" {
						state["resolved_conflicts"] = []string{}
					}
					if c.Seed == "PREPARING" {
						delete(state, "conflicts")
						patch, e := JobPatch(worker, snapshot)
						if e != nil {
							t.Fatal(e)
						}
						if e = os.WriteFile(filepath.Join(recovery, "worker.patch"), []byte(patch), 0600); e == nil {
							err = copyReplayTree(worker, filepath.Join(recovery, "worker-tree"))
						}
					}
					if c.Seed == "RESOLVING" {
						err = copyReplayTree(worker, filepath.Join(recovery, "worker-tree"))
					}
					if err == nil {
						var b []byte
						b, err = json.Marshal(state)
						if err == nil {
							err = os.WriteFile(filepath.Join(recovery, "state.json"), b, 0600)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			resolver := func(conflicts []string, current string) (bool, error) {
				switch c.Resolver {
				case "clean":
					if err := os.WriteFile(filepath.Join(worker, "tracked.txt"), []byte("resolved\n"), 0600); err != nil {
						return false, err
					}
				case "markers":
					if err := os.WriteFile(filepath.Join(worker, "tracked.txt"), []byte("<<<<<<< ours\n=======\n>>>>>>> theirs\n"), 0600); err != nil {
						return false, err
					}
				case "raises":
					return false, os.ErrInvalid
				}
				return true, nil
			}
			runner := replayGitRunner(runGit)
			if c.Script == "ls_tree_error" {
				runner = func(root string, args []string, timeout time.Duration, operation string, input []byte, env map[string]string) ([]byte, error) {
					if operation == "checking replay source path" {
						return nil, &inspectionError{"GIT_FAILED", "scripted ls-tree inspection failure"}
					}
					return runGit(root, args, timeout, operation, input, env)
				}
			}
			got, runErr := prepareJobReplay(repo, worker, snapshot, recovery, resolver, runner)
			if runErr != nil {
				t.Fatal(runErr)
			}
			normalizationTarget := target
			if target == snapshot && (c.Result["status"] != "prepared" || c.Seed == "PREPARED") {
				normalizationTarget = ""
			}
			if err = assertReplayFixture(got, c.Result, snapshot, normalizationTarget); err != nil {
				t.Fatal(err)
			}
			if err = assertReplayFixture(replayArtifacts(t, recovery), c.Recovery, snapshot, normalizationTarget); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func applyReplayChanges(t *testing.T, root string, changes []map[string]string) {
	t.Helper()
	for _, c := range changes {
		writePatchTestFile(t, filepath.Join(root, filepath.FromSlash(c["path"])), []byte(c["content"]))
	}
}

func initReplayRepository(t *testing.T, root, tracked string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q")
	if tracked == "" {
		tracked = "base\n"
	}
	writePatchTestFile(t, filepath.Join(root, "tracked.txt"), []byte(tracked))
	writePatchTestFile(t, filepath.Join(root, "other.txt"), []byte("other\n"))
	git(t, root, "add", "--all")
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	git(t, root, "config", "core.autocrlf", "false")
	return stringsOutput(t, root, "rev-parse", "HEAD^{tree}")
}
func readReplayState(t *testing.T, root string) map[string]any {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(root, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	var s map[string]any
	if e = json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func replayArtifacts(t *testing.T, root string) map[string]any {
	t.Helper()
	files := []string{}
	state := any(nil)
	patch := false
	tree := []any{}
	entries, e := os.ReadDir(root)
	if e == nil {
		for _, x := range entries {
			files = append(files, x.Name())
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(root, "worker.patch")); e == nil {
		patch = true
	}
	if _, e = os.Stat(filepath.Join(root, "state.json")); e == nil {
		b, x := os.ReadFile(filepath.Join(root, "state.json"))
		var decoded map[string]any
		if x == nil {
			x = json.Unmarshal(b, &decoded)
		}
		if x == nil {
			state = decoded
		} else {
			state = "<UNREADABLE>"
		}
	}
	base := filepath.Join(root, "worker-tree")
	if e = filepath.WalkDir(base, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		i, e := os.Lstat(p)
		if e != nil {
			return e
		}
		r, _ := filepath.Rel(base, p)
		if i.Mode()&os.ModeSymlink != 0 {
			v, x := os.Readlink(p)
			if x == nil {
				tree = append(tree, map[string]any{"path": filepath.ToSlash(r), "symlink": v})
			}
			return x
		}
		if i.Mode().IsRegular() {
			b, x := os.ReadFile(p)
			if x != nil {
				return x
			}
			h := sha256.Sum256(b)
			tree = append(tree, map[string]any{"path": filepath.ToSlash(r), "sha256": hex.EncodeToString(h[:])})
		}
		return nil
	}); e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	return map[string]any{"files": files, "state": state, "worker_patch": patch, "worker_tree": tree}
}
func assertReplayFixture(got, want any, snapshot, target string) error {
	a, _ := json.Marshal(normalizeReplay(got, snapshot, target))
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		return &replayMismatch{string(a), string(b)}
	}
	return nil
}
func normalizeReplay(v any, snapshot, target string) any {
	switch x := v.(type) {
	case map[string]any:
		o := map[string]any{}
		for k, v := range x {
			o[k] = normalizeReplay(v, snapshot, target)
		}
		return o
	case []any:
		o := make([]any, len(x))
		for i, v := range x {
			o[i] = normalizeReplay(v, snapshot, target)
		}
		return o
	case []string:
		o := make([]any, len(x))
		for i, v := range x {
			o[i] = normalizeReplay(v, snapshot, target)
		}
		return o
	case string:
		if x == target && target != "" {
			return "<TARGET>"
		}
		if x == snapshot {
			return "<SNAPSHOT>"
		}
		return x
	}
	return v
}

type replayMismatch struct{ got, want string }

func (e *replayMismatch) Error() string {
	return "replay fixture mismatch\n got: " + e.got + "\nwant: " + e.want
}
