package initsetup

import (
	"bytes"
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
