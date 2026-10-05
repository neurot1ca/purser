package proc

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
)

type pythonLocatorResult struct {
	Path string `json:"path"`
}

// ResolvePython uses the plugin's locator, with the same PATH fallback used by code map.
func ResolvePython(ctx context.Context, pluginRoot string) string {
	node, err := exec.LookPath("node")
	locator := filepath.Join(pluginRoot, "bin", "python-locator.js")
	pythonPath := ""
	if err == nil {
		cmd := exec.CommandContext(ctx, node, locator, "--resolve-json")
		cmd.Env = os.Environ()
		if data, callErr := cmd.Output(); callErr == nil {
			var python pythonLocatorResult
			if json.Unmarshal(data, &python) == nil {
				pythonPath = python.Path
			}
		}
	}
	if pythonPath == "" {
		pythonPath, _ = exec.LookPath("python")
	}
	if ctx.Err() != nil {
		return ""
	}
	return pythonPath
}
