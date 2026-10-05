package obsidian

import (
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
