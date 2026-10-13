package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"purser-hook/internal/contextio"
	"purser-hook/internal/pluginroot"
	"purser-hook/internal/policy"
)

func gearMain(args []string) int {
	if len(args) == 0 || (args[0] != "up" && args[0] != "down") {
		return gearError(fmt.Errorf("usage: purser-hook gear up|down --cwd <repository>"))
	}
	action := args[0]
	flags := flag.NewFlagSet("gear", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	cwd := flags.String("cwd", "", "repository path")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *cwd == "" {
		return gearError(fmt.Errorf("usage: purser-hook gear up|down --cwd <repository>"))
	}
	repository, err := gearRepository(*cwd)
	if err != nil {
		return gearError(err)
	}
	config, err := gearConfig(repository)
	if err != nil {
		return gearError(err)
	}
	key := contextio.ProjectKey(repository)
	if err := contextio.SetGear(stateRoot(), key, action); err != nil {
		return gearError(err)
	}
	pairs := make(map[string]string)
	for _, route := range policy.GearEligibleRoutes(config) {
		entry, _ := config["routes"].(map[string]any)
		routeConfig, _ := entry[route].(map[string]any)
		model, _ := routeConfig["model"].(string)
		effort, _ := routeConfig["effort"].(string)
		if action == "up" {
			targetModel, targetEffort, changed := policy.GearTarget(config, model, effort)
			if changed {
				pairs[route] = targetModel + "/" + targetEffort
			}
		} else {
			pairs[route] = model + "/" + effort
		}
	}
	ack, err := json.Marshal(map[string]any{"state": action, "scope": "project", "routes": pairs})
	if err != nil {
		return gearError(err)
	}
	_, err = fmt.Fprintln(os.Stdout, string(ack))
	if err != nil {
		return gearError(err)
	}
	return 0
}

func gearRepository(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("--cwd must be a repository directory")
	}
	for dir := absolute; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return absolute, nil
}

func gearConfig(repository string) (map[string]any, error) {
	pluginRoot := pluginroot.Root()
	if pluginRoot == "" {
		return nil, fmt.Errorf("orchestrator config path is unavailable")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return policy.LoadCoderConfigForRepository(filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json"), filepath.Join(home, ".orchestrator", "config.toml"), repository)
}

func gearError(err error) int {
	_, _ = fmt.Fprintf(os.Stderr, "Gear preference could not be updated: %v\n", err)
	return 1
}
