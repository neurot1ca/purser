package maintenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/proc"
)

var checkSleep = time.Sleep
var maintenanceClock = time.Now

func LatestQuota(now time.Time) map[string]any { return latestMaintenanceQuota(now, "codex") }

func CanonicalRepository(repository string) (string, error) {
	root, err := filepath.Abs(repository)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	command := proc.Hide(exec.Command("git", "-C", root, "rev-parse", "--show-toplevel"))
	proc.ConfigureNoWindow(command)
	data, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("not a project repository")
	}
	return filepath.Clean(strings.TrimSpace(string(data))), nil
}

func MaintenanceDirectory(state, root string) string {
	return filepath.Join(state, "projects", contextio.ProjectKey(root), "maintenance")
}

func RequireOptIn(state, root string) error {
	data, err := os.ReadFile(filepath.Join(MaintenanceDirectory(state, root), "opt-in.json"))
	if err != nil {
		return fmt.Errorf("Maintenance is not opted in for %s; run opt-in explicitly first.", root)
	}
	var marker struct {
		Enabled     bool   `json:"enabled"`
		ProjectRoot string `json:"project_root"`
	}
	if json.Unmarshal(data, &marker) != nil || !marker.Enabled || marker.ProjectRoot != root {
		return fmt.Errorf("Maintenance is not opted in for %s; run opt-in explicitly first.", root)
	}
	return nil
}

func writeCommandJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".maintenance-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func Control(state, command, repository, decision, report string, acted []string) (string, error) {
	root, err := CanonicalRepository(repository)
	if err != nil {
		return "", err
	}
	dir := MaintenanceDirectory(state, root)
	marker := filepath.Join(dir, "opt-in.json")
	switch command {
	case "opt-in":
		if err := writeCommandJSON(marker, map[string]any{"project_root": root, "enabled": true}); err != nil {
			return "", err
		}
		consentPath := filepath.Join(dir, "consent.json")
		value := map[string]any{}
		if data, readErr := os.ReadFile(consentPath); readErr == nil {
			_ = json.Unmarshal(data, &value)
		}
		value["project_root"], value["decision"], value["source"] = root, "yes", "manual-opt-in"
		if err := writeCommandJSON(consentPath, value); err != nil {
			return "", err
		}
		return marker, nil
	case "opt-out":
		value := map[string]any{"project_root": root}
		if data, readErr := os.ReadFile(marker); readErr == nil {
			_ = json.Unmarshal(data, &value)
		}
		value["project_root"], value["enabled"] = root, false
		if err := writeCommandJSON(marker, value); err != nil {
			return "", err
		}
		return marker, nil
	case "consent":
		if decision != "yes" && decision != "no" {
			return "", errors.New("consent decision must be yes or no")
		}
		path := filepath.Join(dir, "consent.json")
		if err := writeCommandJSON(path, map[string]any{"project_root": root, "decision": decision}); err != nil {
			return "", err
		}
		if err := writeCommandJSON(marker, map[string]any{"project_root": root, "enabled": decision == "yes"}); err != nil {
			return "", err
		}
		return path, nil
	case "skip-second-show", "mark-done":
		path, err := filepath.Abs(report)
		if err != nil {
			return "", err
		}
		path, _ = filepath.EvalSymlinks(path)
		reportType := filepath.Base(filepath.Dir(path))
		expected := filepath.Join(dir, "reports", reportType)
		expected, _ = filepath.Abs(expected)
		if (reportType != "cleanup" && reportType != "test-gaps") || filepath.Clean(filepath.Dir(path)) != filepath.Clean(expected) || !strings.HasSuffix(filepath.Base(path), ".notice.json") {
			return "", errors.New("report notice must belong to this project")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		var notice map[string]any
		if err := json.Unmarshal(data, &notice); err != nil {
			return "", err
		}
		if command == "skip-second-show" {
			cycle, _ := notice["pending_cycle"].(string)
			if cycle == "" {
				cycle, _ = notice["last_shown_cycle"].(string)
			}
			if cycle == "" || cycle == "no-window" {
				return "", errors.New("no quota cycle is pending")
			}
			cycles, _ := notice["cycles"].(map[string]any)
			if cycles == nil {
				cycles = map[string]any{}
				notice["cycles"] = cycles
			}
			entry, _ := cycles[cycle].(map[string]any)
			if entry == nil {
				entry = map[string]any{}
				cycles[cycle] = entry
			}
			entry["skip_second"] = true
			notice["pending"] = false
			delete(notice, "pending_cycle")
			delete(notice, "pending_show")
		} else {
			findings, _ := notice["findings"].([]any)
			allowed := map[string]bool{}
			for _, raw := range findings {
				if item, ok := raw.(map[string]any); ok {
					if line, ok := item["path_line"].(string); ok {
						allowed[line] = true
					}
				}
			}
			for _, value := range acted {
				if !allowed[value] {
					return "", fmt.Errorf("acted path_line not found in report: %q", value)
				}
			}
			notice["done"], notice["pending"], notice["shown"], notice["acted_findings"] = true, false, true, acted
		}
		return "", writeCommandJSON(path, notice)
	default:
		return "", fmt.Errorf("unsupported maintenance command: %s", command)
	}
}

// DelayedCheck is the detached SessionStart worker. It scans enabled projects,
// honors the requested project, then chooses its oldest due report type.
func DelayedCheck(repository, state string, delay time.Duration, runner Runner) (string, error) {
	checkSleep(minDuration(maxDuration(delay, 0), 5*time.Minute))
	now := maintenanceClock()
	if err := runner.resolveCoder(repository); err != nil {
		return "", err
	}
	quota := latestMaintenanceQuota(now, runner.CoderAgent)
	root, err := CanonicalRepository(repository)
	if err != nil {
		return "", err
	}
	if err := ScanOptedInProjects(root, state); err != nil {
		return "", nil
	}
	dir := MaintenanceDirectory(state, root)
	var due []string
	for _, kind := range []string{"cleanup", "test-gaps"} {
		if contextio.MaintenanceEligible(root, state, now, quota, kind, runner.CoderAgent == "codex").Eligible {
			due = append(due, kind)
		}
	}
	if len(due) == 0 {
		return "", nil
	}
	kind := contextio.SelectMaintenanceReportType(dir, due)
	return RunAndPersistReport(runner, state, root, kind)
}

func ScanOptedInProjects(candidate, state string) error {
	projects := filepath.Join(state, "projects")
	entries, err := os.ReadDir(projects)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	found := false
	for _, entry := range entries {
		path := filepath.Join(projects, entry.Name(), "maintenance", "opt-in.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var marker struct {
			Enabled     bool   `json:"enabled"`
			ProjectRoot string `json:"project_root"`
		}
		if json.Unmarshal(data, &marker) != nil || !marker.Enabled {
			continue
		}
		root, err := CanonicalRepository(marker.ProjectRoot)
		if err == nil && root == marker.ProjectRoot && MaintenanceDirectory(state, root) == filepath.Dir(path) && root == candidate {
			found = true
		}
	}
	if !found {
		return errors.New("project not opted in")
	}
	return nil
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
