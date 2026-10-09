package initsetup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/policy"
	"purser-hook/internal/profile"
)

func TestClaudeCoderPlanAndApply(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	configPath := filepath.Join(home, ".orchestrator", "config.toml")
	shippedConfig, err := filepath.Abs(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	var plan bytes.Buffer
	if code := Run(&plan, &bytes.Buffer{}, []string{"--plan", "--coder", "claude", "--reviewer", "agy"}); code != 0 {
		t.Fatalf("plan failed: %s", plan.String())
	}
	if strings.Contains(strings.ToLower(plan.String()), "python") {
		t.Errorf("plan unexpectedly mentions Python:\n%s", plan.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".orchestrator", "python")); !os.IsNotExist(err) {
		t.Errorf("plan unexpectedly created ~/.orchestrator/python (stat error: %v)", err)
	}
	for _, route := range []string{
		"mechanical -> haiku / none", "subtle -> sonnet / medium", "normal -> sonnet / medium",
		"consequential -> opus / high", "escalated -> opus / xhigh", "operation -> haiku / none",
		"plan_standard -> opus / high",
	} {
		if !strings.Contains(plan.String(), route) {
			t.Errorf("plan does not list %q:\n%s", route, plan.String())
		}
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	prior := "schema_version = 1\n\n[roles]\ncoder = \"codex\"\n\n[coding]\ncoder_agent = \"codex\"\n\n[coding.routes.normal]\nmodel = \"gpt-6.1-sol\"\neffort = \"medium\"\n"
	if err := os.WriteFile(configPath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	pythonPath := filepath.Join(home, ".orchestrator", "python")
	pythonPrior := []byte("user-owned locator\r\nwith exact bytes\n")
	if err := os.WriteFile(pythonPath, pythonPrior, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, []string{"--apply", "--coder", "claude", "--reviewer", "agy"}); code != 0 {
		t.Fatal("apply failed")
	}
	pythonAfter, err := os.ReadFile(pythonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pythonAfter, pythonPrior) {
		t.Errorf("existing ~/.orchestrator/python changed: got %q, want %q", pythonAfter, pythonPrior)
	}
	loaded, err := policy.LoadCoderConfig(shippedConfig, configPath)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if got := loaded["coder_agent"]; got != "claude" {
		t.Errorf("coder_agent = %v, want claude", got)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := profile.DecodeTOML(raw)
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := user["roles"].(map[string]any)
	if roles["coder"] != "claude" {
		t.Errorf("roles.coder = %v, want claude", roles["coder"])
	}
	coding, _ := user["coding"].(map[string]any)
	if _, exists := coding["routes"]; exists {
		t.Error("Claude config should use built-in routes without a [coding.routes] table")
	}
}

func TestAutoCompactInit(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	marker := filepath.Join(state, "auto-compact.json")
	baseArgs := []string{"--coder", "claude", "--reviewer", "agy"}

	for _, tc := range []struct {
		flag string
		want bool
	}{{"--auto-compact", true}, {"--no-auto-compact", false}} {
		t.Run(tc.flag, func(t *testing.T) {
			planArgs := append([]string{"--plan"}, append(baseArgs, tc.flag)...)
			var plan bytes.Buffer
			if code := Run(&plan, &bytes.Buffer{}, planArgs); code != 0 {
				t.Fatalf("plan failed with %d: %s", code, plan.String())
			}
			wantPlan := "Automatic /compact: disabled for this user"
			if tc.want {
				wantPlan = "Automatic /compact: enabled for this user"
			}
			if !strings.Contains(plan.String(), wantPlan) {
				t.Errorf("plan missing %q:\n%s", wantPlan, plan.String())
			}
			applyArgs := append([]string{"--apply"}, append(baseArgs, tc.flag)...)
			if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, applyArgs); code != 0 {
				t.Fatalf("apply failed with %d", code)
			}
			var saved struct {
				Enabled bool `json:"enabled"`
			}
			raw, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("invalid auto-compact marker %q: %v", raw, err)
			}
			if saved.Enabled != tc.want {
				t.Errorf("saved enabled = %t, want %t", saved.Enabled, tc.want)
			}
		})
	}

	original := []byte("{\"enabled\":true}\n")
	if err := os.WriteFile(marker, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, append([]string{"--apply"}, baseArgs...)); code != 0 {
		t.Fatalf("apply without auto-compact choice failed with %d", code)
	}
	after, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("apply without choice changed auto-compact file: got %q, want %q", after, original)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, append(append([]string{"--plan"}, baseArgs...), "--auto-compact", "--no-auto-compact")); code != 2 {
		t.Errorf("conflicting auto-compact flags returned %d, want usage error 2", code)
	}

	for _, want := range []any{nil, true, false} {
		if want != nil {
			raw, _ := json.Marshal(map[string]bool{"enabled": want.(bool)})
			if err := os.WriteFile(marker, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		var output bytes.Buffer
		args := []string{"--detect", "--json"}
		if code := Run(&output, &bytes.Buffer{}, args); code != 0 {
			t.Fatalf("detect failed with %d: %s", code, output.String())
		}
		var detected map[string]any
		if err := json.Unmarshal(output.Bytes(), &detected); err != nil {
			t.Fatalf("invalid detect output %q: %v", output.String(), err)
		}
		if got, ok := detected["auto_compact_enabled"]; !ok || got != want {
			t.Errorf("auto_compact_enabled = %#v (present %t), want %#v", got, ok, want)
		}
	}
}

func TestEnableConciseUsesActiveClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct {
		name, configDir string
	}{
		{name: "explicit", configDir: filepath.Join(t.TempDir(), "claude-config")},
		{name: "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tc.configDir)
			dir := tc.configDir
			if dir == "" {
				dir = filepath.Join(home, ".claude")
			}
			var out, errOut bytes.Buffer
			if code := enableConcise(&out, &errOut); code != 0 {
				t.Fatalf("enableConcise returned %d: %s", code, errOut.String())
			}
			data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(data, &settings); err != nil || settings["outputStyle"] != "Concise" {
				t.Fatalf("settings = %s, error = %v", data, err)
			}
		})
	}
}
