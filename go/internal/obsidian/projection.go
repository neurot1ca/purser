package obsidian

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// PluginRoot is the shared source of the installed plugin package root.
func PluginRoot() string {
	if root := os.Getenv("CLAUDE_PLUGIN_ROOT"); root != "" {
		return root
	}
	cwd, _ := os.Getwd()
	return cwd
}

// ConfigPath prefers the plugin's bundled config and falls back to the cwd ancestry.
func ConfigPath() string {
	pluginConfig := filepath.Join(PluginRoot(), ".claude", "hooks", "orchestrator", "config.json")
	if _, err := os.Stat(pluginConfig); err == nil {
		return pluginConfig
	}
	working, err := os.Getwd()
	if err != nil {
		return pluginConfig
	}
	for dir := working; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".claude", "hooks", "orchestrator", "config.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return candidate
		}
	}
}

// ProjectionSettings mirrors the vault resolution used by task-note projection.
func ProjectionSettings(configData []byte, envVault, explicitVault *string) (bool, string) {
	var config map[string]any
	if json.Unmarshal(configData, &config) != nil || config == nil {
		return false, ""
	}
	setting, hasSetting := config["obsidian_projection"]
	enabled, configured := false, ""
	switch value := setting.(type) {
	case map[string]any:
		enabled, _ = value["enabled"].(bool)
		configured, _ = value["vault_root"].(string)
	case bool:
		enabled = value
		configured, _ = config["obsidian_vault_root"].(string)
	default:
		enabled, _ = config["obsidian_projection_enabled"].(bool)
		configured, _ = config["obsidian_vault_root"].(string)
	}
	if explicitVault != nil {
		return true, absoluteVault(*explicitVault)
	}
	if !enabled {
		return false, ""
	}
	if envVault != nil {
		configured = *envVault
	} else if hasSetting {
		if nested, ok := setting.(map[string]any); ok && nested["vault_root"] == nil {
			configured, _ = config["obsidian_vault_root"].(string)
		}
	}
	root := absoluteVault(configured)
	return root != "", root
}

func absoluteVault(value string) string {
	if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) {
		return ""
	}
	return filepath.Clean(value)
}
