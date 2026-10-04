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
	Loads []struct {
		Input struct {
			Name         string  `json:"name"`
			DefaultUser  bool    `json:"default_user"`
			User         *string `json:"user"`
			Project      *string `json:"project"`
			OuterFailure bool    `json:"outer_failure"`
		} `json:"input"`
		Output ResolvedProfile `json:"output"`
	} `json:"loads"`
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

func TestLoadProfilePythonFixture(t *testing.T) {
	for _, test := range readResolveFixture(t).Loads {
		t.Run(test.Input.Name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			if err := os.MkdirAll(filepath.Join(home, ".orchestrator"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(root, test.Input.Name+".toml")
			if test.Input.DefaultUser {
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				userPath = filepath.Join(home, ".orchestrator", "config.toml")
			}
			if test.Input.User != nil {
				content := strings.ReplaceAll(*test.Input.User, "$ROOT", filepath.ToSlash(root))
				if err := os.WriteFile(userPath, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			projectPath := filepath.Join(root, ".orchestrator", "profile.toml")
			if test.Input.Project != nil {
				if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
					t.Fatal(err)
				}
				content := strings.ReplaceAll(*test.Input.Project, "$ROOT", filepath.ToSlash(root))
				if err := os.WriteFile(projectPath, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var got ResolvedProfile
			if test.Input.OuterFailure {
				got = loadProfileFiles(root, userPath, func(string) ([]byte, error) { panic("forced recovery") })
			} else if test.Input.DefaultUser {
				got = LoadProfile(root)
			} else {
				got = LoadProfile(root, userPath)
			}
			compareResolved(t, got, test.Output)
		})
	}
}

func TestLoadProfileReadAndParseFailuresRejectTheirTier(t *testing.T) {
	root := t.TempDir()
	userPath := filepath.Join(root, "unreadable")
	if err := os.Mkdir(userPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := LoadProfile(root, userPath); got.UserStatus != "rejected" || got.Trusted {
		t.Fatalf("read failure was trusted: %+v", got)
	}
	if err := os.Remove(userPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("schema_version = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadProfile(root, userPath); got.UserStatus != "rejected" || got.Trusted || len(got.Warnings) != 1 || !strings.HasPrefix(got.Warnings[0], "user tier rejected: could not parse profile: ") {
		t.Fatalf("parse failure did not reach its rejection branch: %+v", got)
	}
}

func compareResolved(t *testing.T, got, want ResolvedProfile) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	var actual, expected any
	_ = json.Unmarshal(gotJSON, &actual)
	_ = json.Unmarshal(wantJSON, &expected)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("LoadProfile: got %s, want %s", gotJSON, wantJSON)
	}
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
