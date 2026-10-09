package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPythonConfigGoldenParity(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "config_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string         `json:"name"`
		Mode   string         `json:"mode"`
		Input  map[string]any `json:"input"`
		OK     bool           `json:"ok"`
		Error  string         `json:"error"`
		Loaded map[string]any `json:"loaded"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if tc.Mode != "" {
			continue
		}
		t.Run(tc.Name, func(t *testing.T) {
			err := ValidateConfig(tc.Input)
			if (err == nil) != tc.OK {
				t.Fatalf("ValidateConfig error = %v, want ok=%v", err, tc.OK)
			}
			if err != nil && err.Error() != tc.Error {
				t.Fatalf("ValidateConfig error = %q, want %q", err, tc.Error)
			}
			loaded, err := LoadConfig(tc.Input)
			if !tc.OK {
				if err == nil || err.Error() != tc.Error {
					t.Fatalf("LoadConfig error = %v, want %q", err, tc.Error)
				}
				return
			}
			if err != nil || !jsonEqual(loaded, tc.Loaded) {
				t.Fatalf("LoadConfig = %#v, %v; want %#v", loaded, err, tc.Loaded)
			}
		})
	}
}

func TestPythonCoderConfigGoldenParity(t *testing.T) {
	testPythonCoderConfigGoldenParity(t, false)
	if runtime.GOOS == "linux" {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[coding]\nreviewer1_executable = \"C:/Review/reviewer.exe\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCoderConfig(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"), path); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("foreign Windows executable accepted on Linux: %v", err)
		}
	}
}

func TestPythonCoderConfigGoldenParityP32(t *testing.T) {
	testPythonCoderConfigGoldenParity(t, true)
}

func TestClaudeFallbackAndReviewerChoicesValidateAndPersist(t *testing.T) {
	testRoot := filepath.Join("..", "..", "..", ".tmp", "claude-role-config")
	if err := os.RemoveAll(testRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(testRoot, ".orchestrator"), 0700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(testRoot)
	configPath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json")
	userPath := filepath.Join(testRoot, ".orchestrator", "config.toml")
	valid := "[coding]\nreviewer1_agent = \"claude\"\nreviewer1_model_requested = \"claude-sonnet-4-5\"\nreviewer1_effort = \"high\"\n\n[fallback]\nagent = \"claude\"\nmodel = \"claude-sonnet-4-5\"\nfamily = \"anthropic\"\neffort = \"high\"\n"
	if err := os.WriteFile(userPath, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCoderConfig(configPath, userPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded["reviewer1_agent"] != "claude" || loaded["reviewer1_model_requested"] != "claude-sonnet-4-5" || loaded["reviewer1_effort"] != "high" || loaded["fallback"].(map[string]any)["effort"] != "high" {
		t.Fatalf("Claude role settings were not retained: %#v", loaded)
	}
	invalid := strings.Replace(valid, `effort = "high"`, `effort = "unbounded"`, 1)
	if err := os.WriteFile(userPath, []byte(invalid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCoderConfig(configPath, userPath); err == nil {
		t.Fatal("unsupported Claude effort was accepted")
	}
}

func TestClaudeCoderUsesTierAliasesAndRequiresClaudeRole(t *testing.T) {
	testRoot := filepath.Join("..", "..", "..", ".tmp", "claude-coder-config")
	if err := os.MkdirAll(filepath.Join(testRoot, ".orchestrator"), 0700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(testRoot)
	configPath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json")
	userPath := filepath.Join(testRoot, ".orchestrator", "config.toml")
	if err := os.WriteFile(userPath, []byte("[roles]\ncoder = \"claude\"\n\n[coding]\ncoder_agent = \"claude\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCoderConfig(configPath, userPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"escalated": {"opus", "xhigh"}, "plan_standard": {"opus", "high"},
		"normal": {"sonnet", "medium"}, "mechanical": {"haiku", "none"},
		"operation": {"haiku", "none"}, "subtle": {"sonnet", "medium"},
		"consequential": {"opus", "high"},
	}
	for route, pair := range want {
		got := ResolveRoute(route, loaded, false)
		if !got.Valid || got.Model == nil || *got.Model != pair[0] || got.Effort == nil || *got.Effort != pair[1] {
			t.Errorf("ResolveRoute(%q) = %#v, want %s/%s", route, got, pair[0], pair[1])
		}
	}

	if err := os.WriteFile(userPath, []byte("[roles]\ncoder = \"codex\"\n\n[coding]\ncoder_agent = \"claude\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCoderConfig(configPath, userPath); err == nil || err.Error() != "[coding] coder routes or role are invalid" {
		t.Errorf("mismatched Claude role error = %v", err)
	}

	invalidRoutes := "[roles]\ncoder = \"claude\"\n\n[coding]\ncoder_agent = \"claude\"\n\n[coding.routes.mechanical]\nmodel = \"haiku\"\neffort = \"none\"\n\n[coding.routes.operation]\nmodel = \"haiku\"\neffort = \"none\"\n\n[coding.routes.subtle]\nmodel = \"sonnet\"\neffort = \"medium\"\n\n[coding.routes.normal]\nmodel = \"gpt-x\"\neffort = \"medium\"\n\n[coding.routes.consequential]\nmodel = \"opus\"\neffort = \"high\"\n\n[coding.routes.plan_standard]\nmodel = \"opus\"\neffort = \"high\"\n\n[coding.routes.escalated]\nmodel = \"opus\"\neffort = \"xhigh\"\n"
	if err := os.WriteFile(userPath, []byte(invalidRoutes), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCoderConfig(configPath, userPath); err == nil || !strings.Contains(err.Error(), "INVALID_ROUTE_ENTRY") {
		t.Errorf("non-Claude route error = %v, want INVALID_ROUTE_ENTRY", err)
	}
	customRoutes := strings.Replace(invalidRoutes, `model = "gpt-x"`, `model = "claude-sonnet-4-5"`, 1)
	customRoutes = strings.Replace(customRoutes, `effort = "none"`, `effort = ""`, 1)
	if err := os.WriteFile(userPath, []byte(customRoutes), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadCoderConfig(configPath, userPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveRoute("normal", loaded, false); !got.Valid || got.Model == nil || *got.Model != "claude-sonnet-4-5" {
		t.Errorf("user-supplied Claude model route = %#v", got)
	}
	if got := ResolveRoute("mechanical", loaded, false); !got.Valid || got.Effort == nil || *got.Effort != "none" {
		t.Errorf("empty Claude route effort was not normalized to none: %#v", got)
	}
}

func testPythonCoderConfigGoldenParity(t *testing.T, p32Only bool) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "config_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string         `json:"name"`
		Mode     string         `json:"mode"`
		UserTOML *string        `json:"user_toml"`
		Changes  map[string]any `json:"changes"`
		OK       bool           `json:"ok"`
		Error    string         `json:"error"`
		Loaded   map[string]any `json:"loaded"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json")
	testRootName := "m3-p31-go"
	if p32Only {
		testRootName = "m3-p32-go"
	}
	testRoot := filepath.Join("..", "..", "..", ".tmp", testRootName)
	testRoot, err = filepath.Abs(testRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(testRoot, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		if tc.Mode != "coder" && tc.Mode != "hook" {
			continue
		}
		if p32Only && !strings.Contains(tc.Name, "p32") {
			continue
		}
		t.Run(tc.Name, func(t *testing.T) {
			caseRoot := filepath.Join(testRoot, tc.Name)
			if err := os.RemoveAll(caseRoot); err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(caseRoot, ".orchestrator", "config.toml")
			if tc.UserTOML != nil {
				if err := os.MkdirAll(filepath.Dir(userPath), 0700); err != nil {
					t.Fatal(err)
				}
				executable := strings.ReplaceAll(filepath.Join(caseRoot, "reviewer"), `\`, `\\`)
				content := strings.ReplaceAll(*tc.UserTOML, "__REVIEWER_EXECUTABLE__", executable)
				if err := os.WriteFile(userPath, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := LoadCoderConfig(configPath, userPath)
			if tc.Mode == "hook" && err == nil {
				for key, value := range tc.Changes {
					loaded[key] = value
				}
				err = validateHookConfig(loaded)
			}
			if (err == nil) != tc.OK {
				t.Fatalf("load error = %v, want ok=%v", err, tc.OK)
			}
			if err != nil && err.Error() != tc.Error {
				t.Fatalf("load error = %q, want %q", err, tc.Error)
			}
			want := tc.Loaded
			if tc.Name == "reviewer-settings" {
				want = make(map[string]any, len(tc.Loaded))
				for key, value := range tc.Loaded {
					want[key] = value
				}
				want["reviewer1_executable"] = filepath.Join(caseRoot, "reviewer")
			}
			if err == nil && !jsonEqual(loaded, want) {
				t.Fatalf("loaded config differs:\n got %#v\nwant %#v", loaded, want)
			}
		})
	}
}

func jsonEqual(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}
