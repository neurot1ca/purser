package review

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func materialGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
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

func strPtr(s string) *string { return &s }
