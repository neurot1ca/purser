package enforce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPreParityFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/pre.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Keys   []struct {
			Input    string `json:"input"`
			Expected string `json:"expected"`
		} `json:"project_keys"`
		Roots []struct {
			Name     string `json:"name"`
			Explicit string `json:"explicit"`
			Input    string `json:"input"`
			Event    string `json:"event"`
			Expected string `json:"expected"`
			Reason   string `json:"reason"`
		} `json:"root_cases"`
		Threads []struct {
			ID       any    `json:"thread_id"`
			Expected string `json:"expected"`
		} `json:"thread_cases"`
		Indexes []struct {
			Name     string `json:"name"`
			ID       string `json:"thread_id"`
			Index    any    `json:"index"`
			Expected string `json:"expected"`
			Reason   string `json:"reason"`
		} `json:"index_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "Python project_root and codex indexed reply helpers" {
		t.Fatalf("fixture: %v %q", err, fixture.Oracle)
	}
	scratch := preTempDir(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", scratch)
	for _, item := range fixture.Keys {
		var input *string
		if item.Input != "" {
			input = &item.Input
		}
		if got := projectKey(input); got != item.Expected {
			t.Fatalf("project key %v = %q, want %q", input, got, item.Expected)
		}
	}
	for _, item := range fixture.Threads {
		path, ok := threadIndexPath(scratch, item.ID)
		var got *string
		if ok {
			value := filepath.Base(path)
			got = &value
		}
		if !sameOptional(got, item.Expected) {
			t.Fatalf("thread path %v = %v, want %v", item.ID, got, item.Expected)
		}
	}
	roots := map[string]string{}
	for _, name := range []string{"explicit_repo", "input_repo", "event_repo", "outside"} {
		root := filepath.Join(scratch, name)
		if err = os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if name != "outside" {
			if out, e := exec.Command("git", "init", "-q", root).CombinedOutput(); e != nil {
				t.Fatalf("git init: %v %s", e, out)
			}
		}
		roots[name] = root
	}
	roots["invalid"] = "\x00"
	roots["missing_repo"] = filepath.Join(scratch, "missing_repo")
	for _, item := range fixture.Roots {
		event := map[string]any{}
		if item.Input != "" {
			event["tool_input"] = map[string]any{"cwd": roots[item.Input]}
		}
		if item.Event != "" {
			event["cwd"] = roots[item.Event]
		}
		var explicit *string
		if item.Explicit != "" {
			value := roots[item.Explicit]
			explicit = &value
		}
		got, reason := resolveProjectRoot(event, explicit)
		var expected string
		if item.Expected != "" {
			expected = roots[item.Expected]
		}
		if !sameOptional(got, expected) || reason != item.Reason {
			t.Fatalf("%s = %v, %q; want %q, %q", item.Name, got, reason, expected, item.Reason)
		}
	}
	repo := filepath.Join(scratch, "indexed_repo")
	if err = os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, e := exec.Command("git", "init", "-q", repo).CombinedOutput(); e != nil {
		t.Fatalf("git init: %v %s", e, out)
	}
	runtimeRoot := filepath.Join(scratch, "runtime")
	for _, item := range fixture.Indexes {
		path, valid := threadIndexPath(runtimeRoot, item.ID)
		if item.Name != "missing" && valid {
			if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			var contents []byte
			if raw, ok := item.Index.(string); ok {
				contents = []byte(raw)
			} else {
				entry := item.Index.(map[string]any)
				for key, value := range entry {
					if value == "__REPO__" {
						entry[key] = repo
					}
					if value == "__OUTSIDE__" {
						entry[key] = filepath.Dir(repo)
					}
				}
				contents, err = json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = os.WriteFile(path, contents, 0600); err != nil {
				t.Fatal(err)
			}
		}
		got, reason := indexedReplyRoot(runtimeRoot, item.ID)
		var expected string
		if item.Expected != "" {
			expected = repo
		}
		if !sameOptional(got, expected) || reason != item.Reason {
			t.Fatalf("%s = %v, %q; want %q, %q", item.Name, got, reason, expected, item.Reason)
		}
	}
}

func sameOptional(got *string, want string) bool {
	return got == nil && want == "" || got != nil && samePath(*got, want)
}

func preTempDir(t *testing.T) string {
	t.Helper()
	parent := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := os.MkdirTemp(parent, "go-pre-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}
