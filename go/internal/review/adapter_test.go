package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
			executable, err := filepath.Abs(filepath.Join("tools", "agent.exe"))
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
