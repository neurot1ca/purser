package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"purser-hook/internal/contextio"
	"purser-hook/internal/maintenance"
	"purser-hook/internal/proc"
)

var (
	startSessionMaintenance = proc.StartDetached
	contextSessionStart     = contextio.SessionStart
	writeSessionOutput      = func(output []byte) { _, _ = os.Stdout.Write(output) }
)

func handleSessionStart(event map[string]any) {
	if output := contextSessionStart(event); len(output) > 0 {
		writeSessionOutput(output)
	}
	sessionMaintenance(event)
}

func sessionMaintenance(event map[string]any) {
	defer func() { _ = recover() }()
	defer enqueueCleanup(stateRoot())
	repository := contextio.ProjectRoot(event)
	if repository != "" {
		safeSessionMaintenanceStep(func() { launchCatalogRefresh(repository) })
		safeSessionMaintenanceStep(func() { wakeMaintenanceReport(repository) })
	}
}

func launchCatalogRefresh(repository string) {
	if os.Getenv("PURSER_TEST_DISABLE_CHILD_LAUNCH") == "1" {
		return
	}
	root, err := maintenance.CanonicalRepository(repository)
	if err != nil {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Purser: catalog refresh launch failed: %v\n", err)
		return
	}
	state := stateRoot()
	args := []string{executable, "catalog-refresh", "--state-root", state, "--repo", root}
	env := os.Environ()
	stateSet := false
	for i, value := range env {
		key, _, ok := strings.Cut(value, "=")
		if ok && strings.EqualFold(key, "PURSER_STATE_DIR") {
			env[i] = "PURSER_STATE_DIR=" + state
			stateSet = true
		}
	}
	if !stateSet {
		env = append(env, "PURSER_STATE_DIR="+state)
	}
	if err := startSessionMaintenance(args, env, ""); err != nil {
		fmt.Fprintf(os.Stderr, "Purser: catalog refresh launch failed: %v\n", err)
	}
}

func safeSessionMaintenanceStep(step func()) {
	defer func() { _ = recover() }()
	step()
}

func wakeMaintenanceReport(repository string) {
	root, err := maintenance.CanonicalRepository(repository)
	if err != nil {
		return
	}
	markerPath := filepath.Join(maintenance.MaintenanceDirectory(stateRoot(), root), "opt-in.json")
	markerData, err := os.ReadFile(markerPath)
	if err != nil {
		return
	}
	var marker struct {
		Enabled     bool   `json:"enabled"`
		ProjectRoot string `json:"project_root"`
	}
	if json.Unmarshal(markerData, &marker) != nil || !marker.Enabled || marker.ProjectRoot != root {
		return
	}
	executable, err := os.Executable()
	if err != nil {
		return
	}
	env := os.Environ()
	args := []string{executable, "maintenance", "check", "--repo", root}
	_ = startSessionMaintenance(args, env, "")
}
