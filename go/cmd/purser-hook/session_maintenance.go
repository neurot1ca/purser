package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/jobs"
	"purser-hook/internal/maintenance"
	"purser-hook/internal/models"
	"purser-hook/internal/proc"
)

var (
	discoverSessionModelVersion = models.DiscoverModelVersion
	cleanupSessionJobs          = jobs.CleanupJobs
	startSessionMaintenance     = proc.StartDetached
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
