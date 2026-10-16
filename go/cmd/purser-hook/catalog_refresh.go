package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/maintenance"
	"purser-hook/internal/models"
)

var discoverCatalogModels = models.DiscoverModelVersionForCatalogRefresh
var loadCatalogConfig = gearConfig

func catalogRefreshMain(argv []string) int {
	flags := flag.NewFlagSet("catalog-refresh", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	state := flags.String("state-root", "", "Purser state directory")
	repository := flags.String("repo", "", "canonical repository")
	if err := flags.Parse(argv); err != nil || flags.NArg() != 0 || *state == "" || *repository == "" {
		return catalogRefreshFailure(*state, errors.New("usage: purser-hook catalog-refresh --state-root <path> --repo <repository>"))
	}
	root, err := filepath.Abs(*state)
	if err != nil {
		return catalogRefreshFailure(*state, err)
	}
	canonical, err := maintenance.CanonicalRepository(*repository)
	if err != nil || canonical != filepath.Clean(*repository) {
		return catalogRefreshFailure(root, errors.New("repository is not canonical"))
	}
	if err := os.Setenv("PURSER_STATE_DIR", root); err != nil {
		return catalogRefreshFailure(root, err)
	}
	unlock, err := tryCatalogRefreshLock(filepath.Join(root, "catalog-refresh.lock"))
	if err != nil {
		return 0
	}
	defer unlock()
	if err := refreshCatalog(root, canonical); err != nil {
		return catalogRefreshFailure(root, err)
	}
	_ = os.Remove(filepath.Join(root, "catalog-refresh-error.json"))
	return 0
}

func refreshCatalog(state, repository string) error {
	_, _, _ = discoverCatalogModels(repository)
	config, err := loadCatalogConfig(repository)
	if err != nil {
		return fmt.Errorf("load coder config: %w", err)
	}
	openCodeUsed := catalogUsesOpenCode(config)
	data, err := os.ReadFile(filepath.Join(state, "model-resolution.json"))
	if err != nil {
		if openCodeUsed {
			return fmt.Errorf("OpenCode model snapshot missing after today's listing attempt")
		}
		return nil
	}
	var snapshot struct {
		Date      string              `json:"opencode_snapshot_on"`
		Attempted string              `json:"opencode_attempted_on"`
		Error     string              `json:"opencode_error"`
		Agent     map[string][]string `json:"agents"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("read OpenCode snapshot: %w", err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if openCodeUsed && snapshot.Attempted == today && snapshot.Date != today {
		reason := snapshot.Error
		if reason == "" {
			reason = "OpenCode listing produced no snapshot"
		}
		return fmt.Errorf("OpenCode model snapshot missing: %s", reason)
	}
	ids := freeOpenCodeModels(snapshot.Agent["opencode"])
	if snapshot.Date == "" || len(ids) == 0 {
		return nil
	}
	if config["mechanical_opencode_enabled"] == false {
		return nil
	}
	if date, err := mechanicalInventoryDate(state); err == nil && date == snapshot.Date {
		return bootstrapIfUnmeasured(state)
	}
	if err := enforce.ReconcileMechanicalInventory(state, ids, snapshot.Date); err != nil {
		return fmt.Errorf("reconcile mechanical inventory: %w", err)
	}
	return bootstrapIfUnmeasured(state)
}

func catalogUsesOpenCode(config map[string]any) bool {
	if config["mechanical_opencode_enabled"] != false {
		return true
	}
	if coder, _ := config["coder_agent"].(string); coder == "opencode" {
		return true
	}
	if reviewer, _ := config["reviewer1_agent"].(string); reviewer == "opencode" {
		return true
	}
	if fallback, ok := config["fallback"].(map[string]any); ok {
		if agent, _ := fallback["agent"].(string); agent == "opencode" {
			return true
		}
	}
	return false
}

var bootstrapMechanicalRace = enforce.BootstrapMechanicalRace

func bootstrapIfUnmeasured(state string) error {
	if err := bootstrapMechanicalRace(state); err != nil {
		return fmt.Errorf("bootstrap mechanical race: %w", err)
	}
	return nil
}

func tryCatalogRefreshLock(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := contextio.TryLock(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() error {
		unlockErr := contextio.Unlock(file)
		closeErr := file.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}

func mechanicalInventoryDate(state string) (string, error) {
	data, err := os.ReadFile(filepath.Join(state, "opencode", "candidates.json"))
	if err != nil {
		return "", err
	}
	var inventory struct {
		Date string `json:"catalog_date"`
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		return "", err
	}
	return inventory.Date, nil
}

func freeOpenCodeModels(ids []string) []string {
	free := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "opencode/big-pickle" || strings.HasPrefix(id, "opencode/") && strings.HasSuffix(id, "-free") {
			free = append(free, id)
		}
	}
	return free
}

func catalogRefreshFailure(state string, err error) int {
	if err == nil {
		err = errors.New("catalog refresh failed")
	}
	message := err.Error()
	fmt.Fprintf(os.Stderr, "Purser: catalog refresh failed: %s\n", message)
	if state != "" {
		root, rootErr := filepath.Abs(state)
		if rootErr == nil && os.MkdirAll(root, 0700) == nil {
			payload, _ := json.Marshal(map[string]string{"error": message})
			tmp := filepath.Join(root, "catalog-refresh-error.json.tmp")
			if os.WriteFile(tmp, append(payload, '\n'), 0600) == nil {
				_ = os.Rename(tmp, filepath.Join(root, "catalog-refresh-error.json"))
			}
		}
	}
	return 1
}
