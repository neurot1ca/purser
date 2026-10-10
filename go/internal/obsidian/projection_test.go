package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPathPrefersPluginRootFromUnrelatedProject(t *testing.T) {
	plugin := t.TempDir()
	project := filepath.Join(t.TempDir(), "unrelated-project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	pluginConfig := filepath.Join(plugin, ".claude", "hooks", "orchestrator", "config.json")
	if err := os.MkdirAll(filepath.Dir(pluginConfig), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginConfig, []byte(`{"obsidian_projection":{"enabled":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	if got := ConfigPath(); got != pluginConfig {
		t.Fatalf("config path = %q, want %q", got, pluginConfig)
	}
}

func TestProjectionSettingsPrecedenceAndAbsoluteVault(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "configured-vault")
	fromEnv := filepath.Join(t.TempDir(), "env-vault")
	config := []byte(fmt.Sprintf(`{"obsidian_projection":{"enabled":true,"vault_root":%q}}`, configured))
	enabled, root := ProjectionSettings(config, nil, nil)
	if !enabled || root != configured {
		t.Fatalf("configured settings = (%v, %q), want (true, %q)", enabled, root, configured)
	}
	envEnabled, envRoot := ProjectionSettings(config, &fromEnv, nil)
	if !envEnabled || envRoot != fromEnv {
		t.Fatalf("environment settings = (%v, %q), want (true, %q)", envEnabled, envRoot, fromEnv)
	}
	explicit, explicitRoot := ProjectionSettings([]byte(`{"obsidian_projection":{"enabled":false}}`), nil, &configured)
	if !explicit || explicitRoot != configured {
		t.Fatalf("explicit settings = (%v, %q), want (true, %q)", explicit, explicitRoot, configured)
	}
	if enabled, root := ProjectionSettings([]byte(`{"obsidian_projection":{"enabled":true,"vault_root":"relative"}}`), nil, nil); enabled || root != "" {
		t.Fatalf("relative vault settings = (%v, %q), want (false, empty)", enabled, root)
	}
}
