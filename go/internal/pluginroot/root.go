package pluginroot

import (
	"os"
	"path/filepath"
)

const configPath = ".claude/hooks/orchestrator/config.json"

// Root returns the Purser plugin root, preferring the configured root, then
// the installed binary location, and finally an ancestor of the working dir.
func Root() string {
	return resolve(os.Getenv("CLAUDE_PLUGIN_ROOT"), os.Executable, os.Getwd)
}

func resolve(configuredRoot string, executablePath func() (string, error), workingDir func() (string, error)) string {
	if configuredRoot != "" {
		return configuredRoot
	}
	if executable, err := executablePath(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			root := filepath.Dir(filepath.Dir(resolved))
			if hasConfig(root) {
				return root
			}
		}
	}
	if cwd, err := workingDir(); err == nil {
		for dir := cwd; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if hasConfig(dir) {
				return dir
			}
		}
	}
	return ""
}

func hasConfig(root string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(configPath)))
	return err == nil
}
