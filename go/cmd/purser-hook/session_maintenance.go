package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/jobs"
	"purser-hook/internal/models"
	"purser-hook/internal/obsidian"
	"purser-hook/internal/proc"
)

var (
	discoverSessionModelVersion = models.DiscoverModelVersion
	cleanupSessionJobs          = jobs.CleanupJobs
	resolveSessionPython        = proc.ResolvePython
	startSessionMaintenance     = proc.StartDetached
	sessionMaintenanceStderr    = func(line string) { _, _ = fmt.Fprintln(os.Stderr, line) }
	contextSessionStart         = contextio.SessionStart
	writeSessionOutput          = func(output []byte) { _, _ = os.Stdout.Write(output) }
)

func handleSessionStart(event map[string]any) {
	if output := contextSessionStart(event); len(output) > 0 {
		writeSessionOutput(output)
	}
	sessionMaintenance(event)
}

func sessionMaintenance(event map[string]any) {
	defer func() { _ = recover() }()
	defer safeSessionMaintenanceStep(func() { cleanupSessionJobs(stateRoot(), time.Now(), true) })
	repository := contextio.ProjectRoot(event)
	if repository != "" {
		safeSessionMaintenanceStep(func() { _, _, _ = discoverSessionModelVersion(repository) })
		safeSessionMaintenanceStep(func() { wakeMaintenanceReport(repository) })
	}
}

func safeSessionMaintenanceStep(step func()) {
	defer func() { _ = recover() }()
	step()
}

func wakeMaintenanceReport(repository string) {
	root, err := filepath.Abs(repository)
	if err != nil {
		return
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = filepath.Clean(resolved)
	}
	projectKey := maintenanceProjectKey(root)
	markerPath := filepath.Join(stateRoot(), "projects", projectKey, "maintenance", "opt-in.json")
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
	pluginRoot := obsidian.PluginRoot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	python := resolveSessionPython(ctx, pluginRoot)
	cancel()
	if python == "" {
		sessionMaintenanceStderr("Purser: maintenance reports need Python; skipped")
		return
	}
	env := append(os.Environ(), "PYTHONUTF8=1")
	args := []string{python, filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "maintenance_reports.py"), "check", "--repo", root}
	_ = startSessionMaintenance(args, env, "")
}

func maintenanceProjectKey(canonicalRoot string) string {
	keyPath := canonicalRoot
	if runtime.GOOS == "windows" {
		keyPath = strings.ToLower(filepath.Clean(keyPath))
	}
	hash := sha256.Sum256([]byte(keyPath))
	return hex.EncodeToString(hash[:])[:16]
}
