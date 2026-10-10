package ledger

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestApplyJobPatchLeavesNewFileIntentToAdd(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitApplyPatchTest(t, root, "init", "-q")
	gitApplyPatchTest(t, root, "add", "tracked.txt")
	gitApplyPatchTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
	patch := "diff --git a/new-worker-test.go b/new-worker-test.go\nnew file mode 100644\nindex 0000000..0000000\n--- /dev/null\n+++ b/new-worker-test.go\n@@ -0,0 +1 @@\n+package worker\n"
	result := ApplyJobPatch(root, patch)
	if result["applied"] != true {
		t.Fatalf("new file patch failed: %#v", result)
	}
	status := gitApplyPatchOutput(t, root, "status", "--short", "--", "new-worker-test.go")
	if !strings.Contains(status, "A new-worker-test.go") {
		t.Fatalf("new file is not visible as intent-to-add: %q", status)
	}
	if cached := gitApplyPatchOutput(t, root, "diff", "--cached", "--", "new-worker-test.go"); cached != "" {
		t.Fatalf("intent-to-add staged file contents: %q", cached)
	}
}

func gitApplyPatchTest(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func gitApplyPatchOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

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
