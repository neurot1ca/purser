package review

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"strings"
	"testing"
)

func materialGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testutil.Hide(exec.Command("git", args...))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=NUL", "GIT_AUTHOR_NAME=Materials Test", "GIT_AUTHOR_EMAIL=materials@example.invalid", "GIT_COMMITTER_NAME=Materials Test", "GIT_COMMITTER_EMAIL=materials@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestMaterialsGolden(t *testing.T) {
	root := t.TempDir()
	materialGit(t, root, "init", "-q")
	materialGit(t, root, "config", "core.autocrlf", "false")
	write := func(name, text string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("AGENTS.md", "# Authority\nRoot authority body.\n## Working scope\nScope body.\n### Proportional validation\nValidation body.\n## Test and comment quality\nQuality body.\n## Unselected heading\nDo not include.\n")
	write("modified.txt", "before\nshared\n")
	write("deleted.txt", "remove me\n")
	write("outside.txt", "outside before\n")
	materialGit(t, root, "add", "AGENTS.md", "modified.txt", "deleted.txt", "outside.txt")
	materialGit(t, root, "commit", "-qm", "baseline")
	baseline := materialGit(t, root, "rev-parse", "HEAD")
	write("modified.txt", "after\nshared\n")
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	write("new.txt", "new complete content\n")
	write("outside.txt", "outside after\n")
	delta := map[string]any{
		"baseline_head":      baseline[:len(baseline)-1],
		"changed_paths":      []any{"modified.txt", "new.txt", "deleted.txt"},
		"ambiguous_paths":    []any{},
		"changed_file_count": 3,
		"changed_line_count": 4,
		"sources":            map[string]any{"scoped_manifest": map[string]any{"paths": []any{"new.txt", "outside.txt"}}},
	}
	classification := map[string]any{"decision": "ISOLATED", "in_scope_paths": []any{"modified.txt", "new.txt", "deleted.txt"}}
	materials, err := BuildMaterials("golden-task", root, delta, classification, strPtr("tests passed"), strPtr("objective detail"), strPtr("criterion detail"), []any{map[string]any{"path": "AGENTS.md", "headings": []any{"Authority", "Working scope", "Proportional validation", "Test and comment quality", "Missing heading"}}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(materials)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "materials", "seam_materials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(actual, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("materials differ from Python oracle\n got: %s\nwant: %s", actual, golden)
	}
}

func TestMaterialsMarksPartialCoverageAndTestCandidates(t *testing.T) {
	root := t.TempDir()
	materialGit(t, root, "init", "-q")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\n")
	write("sample_test.go", "package sample\nfunc TestX(t *testing.T) { t.Fatal(\"old\") }\n")
	write("outside.txt", "before\n")
	materialGit(t, root, "add", ".")
	materialGit(t, root, "-c", "user.email=test@example.invalid", "-c", "user.name=Test", "commit", "-qm", "baseline")
	baseline := strings.TrimSpace(materialGit(t, root, "rev-parse", "HEAD"))
	write("sample_test.go", "package sample\nfunc TestX(t *testing.T) { t.Skip(\"later\") }\n")
	write("outside.txt", "changed\n")
	delta := map[string]any{"baseline_head": baseline, "changed_paths": []any{"sample_test.go", "outside.txt"}, "ambiguous_paths": []any{},
		"changed_file_count": 2, "changed_line_count": 2, "sources": map[string]any{"scoped_manifest": map[string]any{"paths": []any{}}}}
	classification := map[string]any{"decision": "UNKNOWN_CONCURRENT_WRITER", "in_scope_paths": []any{"sample_test.go"}}
	materials, err := BuildMaterials("partial", root, delta, classification, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(materials["validation_summary"].(string), "PARTIAL REVIEW COVERAGE") || !strings.Contains(materials["validation_summary"].(string), "outside.txt") {
		t.Fatalf("partial coverage not identified: %q", materials["validation_summary"])
	}
	if !strings.Contains(materials["validation_summary"].(string), "TEST_SKIP_ADDED candidate at sample_test.go") {
		t.Fatalf("test change candidate missing: %q", materials["validation_summary"])
	}
	delegatedDiff := materials["delegated_diff"].(string)
	if !strings.Contains(delegatedDiff, "diff --git a/outside.txt b/outside.txt\n[[OUT OF SCOPE]]\n") || !strings.Contains(delegatedDiff, "-before\n+changed\n") {
		t.Fatalf("out-of-scope diff was not included and marked: %q", delegatedDiff)
	}
	if got := TestWeakeningCandidates("diff --git a/sample_test.go b/sample_test.go\n@@ -1 +1 @@\n-t.Fatal(\"old\")\n+t.Fatal(\"new\")\n"); len(got) != 0 {
		t.Fatalf("assertion replacement was reported as weakening: %v", got)
	}
}

func TestMaterialsMarksUnknownCoverageWhenChangedPathsAreInvalid(t *testing.T) {
	root := t.TempDir()
	materialGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte("package sample\n"), 0600); err != nil {
		t.Fatal(err)
	}
	materialGit(t, root, "add", "sample.go")
	materialGit(t, root, "-c", "user.email=test@example.invalid", "-c", "user.name=Test", "commit", "-qm", "baseline")
	baseline := strings.TrimSpace(materialGit(t, root, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte("package changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	delta := map[string]any{"baseline_head": baseline, "changed_paths": []any{"sample.go", 7}, "ambiguous_paths": []any{},
		"changed_file_count": 1, "changed_line_count": 1, "sources": map[string]any{"scoped_manifest": map[string]any{"paths": []any{}}}}
	classification := map[string]any{"decision": "UNKNOWN_CONCURRENT_WRITER", "in_scope_paths": []any{"sample.go"}}
	materials, err := BuildMaterials("unknown-coverage", root, delta, classification, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := materials["validation_summary"].(string); !strings.Contains(got, "REVIEW COVERAGE UNKNOWN") || !strings.Contains(got, "FULL Claude review is required.") {
		t.Fatalf("unknown coverage marker missing: %q", got)
	}
}

func strPtr(s string) *string { return &s }
