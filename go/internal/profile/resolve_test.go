package profile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type resolveFixture struct {
	Profiles []struct {
		Input struct {
			User    Tier `json:"user"`
			Project Tier `json:"project"`
		} `json:"input"`
		Output ResolvedProfile `json:"output"`
	} `json:"profiles"`
	Protected []struct {
		Path     string   `json:"path"`
		Patterns []string `json:"patterns"`
		Output   bool     `json:"output"`
	} `json:"protected"`
}

func TestProfileResolutionPythonFixture(t *testing.T) {
	fixture := readResolveFixture(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Profiles {
		for _, tier := range []*Tier{&test.Input.User, &test.Input.Project} {
			if paths, ok := tier.Values["trust.paths"].([]any); ok {
				for i, item := range paths {
					if path, ok := item.(string); ok {
						paths[i] = strings.ReplaceAll(path, "$ROOT", root)
					}
				}
			}
		}
		got := ResolveProfile(root, test.Input.User, test.Input.Project)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(test.Output)
		var actual, want any
		_ = json.Unmarshal(gotJSON, &actual)
		_ = json.Unmarshal(wantJSON, &want)
		if !reflect.DeepEqual(actual, want) {
			t.Errorf("ResolveProfile(%#v, %#v): got %s, want %s", test.Input.User, test.Input.Project, gotJSON, wantJSON)
		}
	}
}

func TestIsProtectedPythonFixture(t *testing.T) {
	for _, test := range readResolveFixture(t).Protected {
		if got := IsProtected(test.Path, test.Patterns); got != test.Output {
			t.Errorf("IsProtected(%q, %#v) = %v, want %v", test.Path, test.Patterns, got, test.Output)
		}
	}
}

func readResolveFixture(t *testing.T) resolveFixture {
	t.Helper()
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_profile", "resolve.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture resolveFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
