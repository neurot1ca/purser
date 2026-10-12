package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAdapterGoldens(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	data, err := os.ReadFile("testdata/adapters/python_oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agy", "opencode", "codex"} {
		t.Run(name, func(t *testing.T) {
			adapter, err := ReviewerAdapter(name)
			if err != nil {
				t.Fatal(err)
			}
			var want adapterGolden
			if err := json.Unmarshal(golden[name], &want); err != nil {
				t.Fatal(err)
			}
			model := map[string]string{"agy": "gemini-3.8-flash-high", "opencode": "google/gemini-3.8-flash-high", "codex": "gpt-5"}[name]
			executable, err := filepath.Abs(filepath.Join("tools", name+".exe"))
			if err != nil {
				t.Fatal(err)
			}
			config := map[string]any{"reviewer1_executable": executable, "reviewer1_model_requested": model, "reviewer1_agent": name}
			schema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}}
			prompt := ReviewPrompt(`C:\tmp\packet.json`)
			var command []string
			if name == "agy" {
				repository, err := filepath.Abs(filepath.Join("tmp", "repo"))
				if err != nil {
					t.Fatal(err)
				}
				command, err = adapter.BuildCommand(prompt, schema, config, CommandOptions{RepositoryRoot: repository})
			} else {
				cwd, err := filepath.Abs(filepath.Join("tmp", "worker"))
				if err != nil {
					t.Fatal(err)
				}
				command, err = adapter.BuildCommand(prompt, schema, config, CommandOptions{WorkingDirectory: cwd})
				want.Argv[9] = cwd
			}
			if err != nil {
				t.Fatal(err)
			}
			want.Argv[0] = executable
			if !equal(command, want.Argv) {
				t.Fatalf("argv = %#v, want %#v", command, want.Argv)
			}
			if !sameStringMap(adapter.Environment(command), want.EnvDelta) {
				t.Fatalf("env = %#v, want %#v", adapter.Environment(command), want.EnvDelta)
			}
			if name == "agy" && (adapter.AllowedModel() != "gemini-3.8-flash-high" || adapter.Family(model) != "google") {
				t.Fatalf("agy model/family = %q/%q", adapter.AllowedModel(), adapter.Family(model))
			}
			if runtime.GOOS != "windows" {
				want.Launch.CreationFlags, want.Launch.HideWindow = 0, false
			}
			if got := adapter.LaunchOptions(); got.CreationFlags != want.Launch.CreationFlags || got.HideWindow != want.Launch.HideWindow {
				t.Fatalf("launch = %#v, want %#v", got, want.Launch)
			}
			if string(want.ConfigFile) != "null" {
				t.Fatalf("oracle config file = %s, want null", want.ConfigFile)
			}
			if configName, configData := adapter.ConfinementConfig(command); configName != "" || configData != nil {
				t.Fatalf("confinement file = %q, %q; Python adapters do not write a separate file", configName, configData)
			}
		})
	}
}

func TestReviewerExecutableUsesConfiguredPathOnlyForMatchingProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	configured := filepath.Join(t.TempDir(), "agy"+executableSuffix())
	got, err := ReviewerExecutable(map[string]any{"reviewer1_executable": configured}, "agy")
	if err != nil || got != configured {
		t.Fatalf("agy executable = %q, %v; want %q", got, err, configured)
	}

	pathDir := t.TempDir()
	claude := filepath.Join(pathDir, "claude"+executableSuffix())
	if err := os.WriteFile(claude, []byte("fake claude"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	got, err = ReviewerExecutable(map[string]any{
		"reviewer1_executable": configured,
		"reviewer1_agent":      "claude",
	}, "claude")
	if err != nil {
		t.Fatalf("claude executable lookup: %v", err)
	}
	want, err := filepath.Abs(claude)
	if err != nil {
		t.Fatal(err)
	}
	if canonical, err := filepath.EvalSymlinks(want); err == nil {
		want = canonical
	}
	if got != want {
		t.Fatalf("claude executable = %q, want PATH result %q", got, want)
	}
}

func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func TestAdapterEnvelopeParser(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	adapter, err := ReviewerAdapter("agy")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := adapter.ParseEnvelope(`{"conversation_id":"c","status":"success","response":"","duration_seconds":0.1,"num_turns":1,"usage":{},"structured_output":{"result":"NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE","validation":"PASS","findings":[]}}`)
	if err != nil || parsed["conversation_id"] != "c" {
		t.Fatalf("parsed envelope = %#v, %v", parsed, err)
	}
}

func TestAgyReviewerFamilyFloorAndResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	cache := `{"checked_on":"` + time.Now().UTC().Format("2006-01-02") + `","agents":{"agy":["gemini-3.9-flash-high"]}}`
	if err := os.WriteFile(filepath.Join(root, "model-resolution.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	a, _ := ReviewerAdapter("agy")
	for _, model := range []string{pinnedModel, "gemini-3.9-flash-high", "gemini-3.7-flash-high", "gemini-3.9-pro-high"} {
		config := map[string]any{"reviewer1_model_requested": model, "reviewer1_timeout_seconds": 60, "reviewer1_packet_max_bytes": 200000, "reviewer1_executable": filepath.Join(root, "agy.exe")}
		valid := model == pinnedModel || model == "gemini-3.9-flash-high"
		if err := a.ValidateModel(model); (err == nil) != valid {
			t.Errorf("adapter %s: %v", model, err)
		}
		if err := ValidateConfig(config); (err == nil) != valid {
			t.Errorf("config %s: %v", model, err)
		}
		if !valid {
			continue
		}
		command, err := a.BuildCommand("review", map[string]any{}, config, CommandOptions{})
		if err != nil || flagValue(command, "--model") != "gemini-3.9-flash-high" {
			t.Errorf("adapter command: %v %v", command, err)
		}
		command, err = buildCommand(Request{Model: model, Executable: filepath.Join(root, "agy.exe")})
		if err != nil || flagValue(command, "--model") != "gemini-3.9-flash-high" {
			t.Errorf("review command: %v %v", command, err)
		}
	}
}

func TestClaudeReviewerLaunchCarriesModelAndEffort(t *testing.T) {
	adapter, err := ReviewerAdapter("claude")
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	config := map[string]any{"reviewer1_executable": filepath.Join(cwd, "claude"), "reviewer1_model_requested": "claude-sonnet-4-5", "reviewer1_effort": "high"}
	command, err := adapter.BuildCommand("review", map[string]any{"type": "object"}, config, CommandOptions{WorkingDirectory: cwd})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command, "\x00")
	if !strings.Contains(joined, "--model\x00claude-sonnet-4-5") || !strings.Contains(joined, "--effort\x00high") || !strings.Contains(joined, "--setting-sources\x00") || !strings.Contains(joined, "--restricted") {
		t.Fatalf("Claude reviewer command did not carry the selected settings or restrictions: %#v", command)
	}
}

func TestCodexReviewerLaunchCarriesAndValidatesEffort(t *testing.T) {
	adapter, err := ReviewerAdapter("codex")
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	config := map[string]any{"reviewer1_executable": filepath.Join(cwd, "codex"), "reviewer1_model_requested": "gpt-6-luna", "reviewer1_effort": "medium"}
	command, err := adapter.BuildCommand("review", map[string]any{"type": "object"}, config, CommandOptions{WorkingDirectory: cwd})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(command, "\x00"), "-c\x00model_reasoning_effort=\"medium\"") {
		t.Fatalf("Codex reviewer command did not carry reasoning effort: %#v", command)
	}
	config["reviewer1_effort"] = "unbounded"
	if _, err := adapter.BuildCommand("review", map[string]any{"type": "object"}, config, CommandOptions{WorkingDirectory: cwd}); err == nil {
		t.Fatal("unsupported Codex reviewer effort was accepted")
	}
}

func TestClaudePromptPrecedesVariadicTools(t *testing.T) {
	adapter, err := ReviewerAdapter("claude")
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	config := map[string]any{"reviewer1_executable": filepath.Join(cwd, "claude"), "reviewer1_model_requested": "claude-haiku-5-5", "reviewer1_effort": "high"}
	prompt := "review packet prompt"
	command, err := adapter.BuildCommand(prompt, map[string]any{"type": "object"}, config, CommandOptions{WorkingDirectory: cwd})
	if err != nil {
		t.Fatal(err)
	}
	promptIndex, toolsIndex := -1, -1
	for i, arg := range command {
		if arg == prompt {
			promptIndex = i
		}
		if arg == "--tools" {
			toolsIndex = i
		}
	}
	if promptIndex < 0 || toolsIndex < 0 || promptIndex > toolsIndex {
		t.Fatalf("Claude prompt must precede variadic --tools arguments: %#v", command)
	}
}

func TestClaudeReviewerAcceptsTierAlias(t *testing.T) {
	adapter, err := ReviewerAdapter("claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ValidateModel("opus"); err != nil {
		t.Fatalf("Claude tier alias rejected: %v", err)
	}
}

func TestClaudeReviewerEffortSurvivesBridgeArgumentValidation(t *testing.T) {
	args := nativeArgs(t.TempDir(), "", "")
	args["reviewer_agent"], args["reviewer_model"], args["reviewer_effort"] = "claude", "claude-sonnet-4-5", "high"
	parsed, err := parseBridgeArguments(args)
	if err != nil || parsed["reviewer_effort"] != "high" {
		t.Fatalf("Claude effort bridge arguments = %#v, %v", parsed, err)
	}
}

type adapterGolden struct {
	Argv       []string          `json:"argv"`
	EnvDelta   map[string]string `json:"env_delta"`
	ConfigFile json.RawMessage   `json:"config_file"`
	Launch     struct {
		CreationFlags uint32 `json:"creationflags"`
		HideWindow    bool   `json:"hide_window"`
	} `json:"launch"`
}

func TestAdapterRefusalsMatchPython(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := RefuseUnsafeFlags([]string{"agy", "--mode", "plan", "--disable-slash-commands"}, ""); err == nil || err.Error() != "--disable-slash-commands silently voids --mode plan; refusing unsafe reviewer command" {
		t.Fatalf("unsafe refusal = %v", err)
	}
	repository, err := filepath.Abs(filepath.Join("tmp", "repo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RefuseWorkingDirectory(filepath.Join(repository, "inside"), repository); err == nil || err.Error() != "the Reviewer-1 working directory must be outside the repository under review" {
		t.Fatalf("cwd refusal = %v", err)
	}
	if err := RefuseUnsafeFlags([]string{"agy", "--add-dir", filepath.Join(repository, "nested")}, repository); err == nil || err.Error() != "--add-dir must never grant Reviewer-1 access to the repository under review" {
		t.Fatalf("repository add-dir refusal = %v", err)
	}
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
