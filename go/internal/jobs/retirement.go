package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/enforce"
	"purser-hook/internal/retention"
)

var errRetirementOwnershipMissing = errors.New("worktree ownership record not found")
var launchRetirementDetached = LaunchDetached

func init() {
	enforce.WorktreeRetirementDispatcher = DispatchWorktreeRetirement
}

func dispatchWorktreeRetirement(stateRoot, jobID string) {
	if err := DispatchWorktreeRetirement(stateRoot, jobID); err != nil {
		log.Printf("worktree retirement dispatch failed for job %s: %v", jobID, err)
	}
}

// DispatchWorktreeRetirement records intent and starts one detached retirement child.
func DispatchWorktreeRetirement(stateRoot, jobID string) error {
	root, err := validateRetirementRoot(stateRoot)
	if err != nil {
		return err
	}
	if !validJobID(jobID) || len(jobID) != 64 {
		return errors.New("invalid retirement job id")
	}
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
	job := readCleanupJSON(jobPath)
	if job == nil || job["jobId"] != jobID {
		return errors.New("retirement job not found")
	}
	worktree, _ := job["worktreePath"].(string)
	if worktree == "" {
		worktree, err = findOwnedWorktree(root, jobID)
		if err != nil {
			if errors.Is(err, errRetirementOwnershipMissing) {
				return nil
			}
			return err
		}
	}
	if !validOwnedWorktree(root, worktree) {
		return errors.New("job has an invalid worktree path")
	}
	prior, err := retention.ReadWorktreeOwnership(root, worktree)
	if err != nil {
		return err
	}
	if !containsOwner(prior.Jobs, jobID) {
		return errors.New("job is not attached to the worktree")
	}
	if !worktreeRetirableNow(root, worktree) {
		return nil
	}
	owned, err := retention.MarkWorktreeRetiring(root, worktree)
	if err != nil {
		return err
	}
	if !containsOwner(owned.Jobs, jobID) {
		return errors.New("job is not attached to the worktree")
	}
	if owned.Claimed {
		return nil
	}
	claimed := false
	owned, err = retention.UpdateWorktreeOwnership(root, worktree, func(record *retention.WorktreeOwnership) {
		if !record.Claimed {
			record.Claimed = true
			claimed = true
		}
	})
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		releaseRetirementClaim(root, worktree)
		return err
	}
	argv := []string{executable, "retire-worktree", root, jobID}
	if generation, ok := cleanupNumber(job["generation"]); ok && generation > 0 {
		argv = append(argv, strconv.FormatInt(int64(generation), 10))
	}
	if _, _, err = launchRetirementDetached(argv, os.Environ(), root); err != nil {
		releaseRetirementClaim(root, worktree)
		return err
	}
	return nil
}

func worktreeRetirableNow(root, worktree string) bool {
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return false
	}
	now := float64(time.Now().UnixNano()) / 1e9
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(jobsDir, entry.Name())
		job := readCleanupJSON(path)
		if job == nil {
			continue
		}
		bound, _ := job["worktreePath"].(string)
		config, _ := job["threadConfig"].(map[string]any)
		nested, _ := config["worktreePath"].(string)
		if (bound == "" || !sameWorktree(bound, worktree)) && (nested == "" || !sameWorktree(nested, worktree)) {
			continue
		}
		state, _ := job["state"].(string)
		if receipt, ok := job["treeReceipt"].(string); ok {
			if stopped, _ := treeStopped(jobsDir, stringValue(job["jobId"]), receipt); !stopped {
				return false
			}
		}
		if !cleanupMayRemove(job, state) || cleanupRecentJobConflict(job, state, now, path) {
			return false
		}
	}
	return true
}

func releaseRetirementClaim(root, worktree string) {
	_, _ = retention.UpdateWorktreeOwnership(root, worktree, func(record *retention.WorktreeOwnership) { record.Claimed = false })
}

// RetireWorktree performs the private one-shot operation. Its targets come only from durable records.
func RetireWorktree(stateRoot, jobID string, generation ...int64) error {
	root, err := validateRetirementRoot(stateRoot)
	if err != nil {
		return err
	}
	if !validJobID(jobID) || len(jobID) != 64 {
		return errors.New("invalid retirement job id")
	}
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
	job := readCleanupJSON(jobPath)
	if job == nil || job["jobId"] != jobID {
		return errors.New("retirement job not found")
	}
	if len(generation) > 1 {
		return errors.New("invalid retirement generation")
	}
	if current, ok := cleanupNumber(job["generation"]); ok && current > 0 {
		if len(generation) != 1 || int64(current) != generation[0] {
			return errors.New("stale retirement generation")
		}
	} else if len(generation) != 0 {
		return errors.New("job has no retirement generation")
	}
	worktree, _ := job["worktreePath"].(string)
	if worktree == "" {
		worktree, err = findOwnedWorktree(root, jobID)
		if err != nil {
			if errors.Is(err, errRetirementOwnershipMissing) {
				return nil
			}
			return err
		}
	}
	if !validOwnedWorktree(root, worktree) {
		return errors.New("job has an invalid worktree path")
	}
	unlock, err := retention.WorktreeRetirementLock(root, worktree)
	if err != nil {
		return err
	}
	defer unlock()
	owned, err := retention.ReadWorktreeOwnership(root, worktree)
	if err != nil {
		return err
	}
	if !owned.Retiring {
		return errors.New("worktree is not retiring")
	}
	if !owned.Claimed {
		return errors.New("worktree retirement is not claimed")
	}
	if filepath.Clean(owned.WorktreePath) != filepath.Clean(worktree) || !validOwnedWorktree(root, owned.WorktreePath) {
		return errors.New("ownership record contains an invalid worktree path")
	}
	if !containsOwner(owned.Jobs, jobID) {
		return errors.New("job is not attached to the worktree")
	}
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	jobIDs := append([]string(nil), owned.Jobs...)
	sort.Strings(jobIDs)
	jobLocks := make(map[string]*OwnershipLock, len(jobIDs))
	defer func() {
		for _, lock := range jobLocks {
			_ = lock.Release()
		}
	}()
	for _, id := range jobIDs {
		lock, lockErr := AcquireOwnership(jobsDir, id)
		if lockErr != nil {
			return lockErr
		}
		jobLocks[id] = lock
	}
	job = readCleanupJSON(jobPath)
	if job == nil || job["jobId"] != jobID {
		return errors.New("retirement job disappeared")
	}
	for _, id := range owned.Jobs {
		if !validJobID(id) || len(id) != 64 {
			return errors.New("ownership record contains an invalid job id")
		}
		p := filepath.Join(jobsDir, id+".json")
		record := readCleanupJSON(p)
		if record == nil || record["jobId"] != id {
			return fmt.Errorf("attached job %s is missing or invalid", id)
		}
		if receipt, exists := record["treeReceipt"]; exists {
			if stopped, stopErr := treeStopped(jobsDir, stringValue(record["jobId"]), stringValue(receipt)); !stopped {
				return stopErr
			}
		}
		state, _ := record["state"].(string)
		if !cleanupEligible(record, state, float64(time.Now().UnixNano())/1e9, p) {
			return fmt.Errorf("attached job %s is not terminal and eligible", id)
		}
		if path, _ := record["worktreePath"].(string); path != "" && !validOwnedWorktree(root, path) {
			return errors.New("attached job contains an invalid worktree path")
		}
	}
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(jobsDir, entry.Name())
		record := readCleanupJSON(path)
		if record == nil {
			continue
		}
		bound, _ := record["worktreePath"].(string)
		config, _ := record["threadConfig"].(map[string]any)
		nestedPath, _ := config["worktreePath"].(string)
		matchesTop := bound != "" && sameWorktree(bound, worktree)
		matchesNested := nestedPath != "" && sameWorktree(nestedPath, worktree) && (bound == "" || matchesTop)
		if !matchesTop && !matchesNested {
			continue
		}
		stem := strings.TrimSuffix(entry.Name(), ".json")
		if jobLocks[stem] == nil {
			lock, lockErr := AcquireOwnership(jobsDir, stem)
			if lockErr != nil {
				return lockErr
			}
			defer lock.Release()
			record = readCleanupJSON(path)
			if record == nil {
				continue
			}
			bound, _ = record["worktreePath"].(string)
			config, _ = record["threadConfig"].(map[string]any)
			nestedPath, _ = config["worktreePath"].(string)
			matchesTop = bound != "" && sameWorktree(bound, worktree)
			matchesNested = nestedPath != "" && sameWorktree(nestedPath, worktree) && (bound == "" || matchesTop)
			if !matchesTop && !matchesNested {
				continue
			}
		}
		if receipt, exists := record["treeReceipt"]; exists {
			if stopped, stopErr := treeStopped(jobsDir, stringValue(record["jobId"]), stringValue(receipt)); !stopped {
				return stopErr
			}
		}
		state, _ := record["state"].(string)
		if !cleanupEligible(record, state, float64(time.Now().UnixNano())/1e9, path) {
			return fmt.Errorf("job %s still references an ineligible worktree", record["jobId"])
		}
		changed := false
		if matchesTop {
			delete(record, "worktreePath")
			delete(record, "snapshot")
			delete(record, "baselineSnapshot")
			delete(record, "head")
			delete(record, "sandbox")
			if repo, _ := record["repoRoot"].(string); repo != "" {
				record["cwd"] = repo
			}
			clearSandboxArgs(record)
			changed = true
		}
		if matchesNested {
			clearNestedThreadConfig(record, worktree, "")
			changed = true
		}
		if changed {
			if err := writeJSONRecord(path, record); err != nil {
				return err
			}
		}
	}
	if err := clearThreadRecords(root, worktree); err != nil {
		return err
	}
	owned, err = retention.ReadWorktreeOwnership(root, worktree)
	if err != nil {
		return err
	}
	if !owned.WorktreeGone {
		repo, _ := job["repoRoot"].(string)
		if repo == "" {
			return errors.New("retirement job has no repository root")
		}
		if pathExists(worktree) {
			if !removeWorktree(root, repo, worktree, map[string]*worktreeListing{}, map[string]string{}) {
				return errors.New("worktree removal failed")
			}
		}
		owned, err = retention.UpdateWorktreeOwnership(root, worktree, func(record *retention.WorktreeOwnership) { record.WorktreeGone = true })
		if err != nil {
			return err
		}
	}
	if !owned.PreparationsGone {
		for _, id := range owned.Jobs {
			prep := filepath.Join(root, "preparations", id)
			if pathExists(prep) && !retention.RemovePreparationTree(root, prep) {
				return fmt.Errorf("preparation removal failed for %s", id)
			}
		}
		owned, err = retention.UpdateWorktreeOwnership(root, worktree, func(record *retention.WorktreeOwnership) { record.PreparationsGone = true })
		if err != nil {
			return err
		}
	}
	return retention.DeleteWorktreeOwnership(root, worktree)
}

func validateRetirementRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("state root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", errors.New("state root must be an existing directory")
	}
	if isCleanupLink(abs) {
		return "", errors.New("state root cannot be a link")
	}
	return filepath.Clean(abs), nil
}

func validOwnedWorktree(root, path string) bool {
	if path == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil || !cleanupPath(root, abs) {
		return false
	}
	wtRoot, target := worktreeKey(filepath.Join(root, "wt")), worktreeKey(abs)
	if wtRoot == "" || target == "" {
		return false
	}
	rel, err := filepath.Rel(wtRoot, target)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sameWorktree(a, b string) bool {
	left, right := worktreeKey(a), worktreeKey(b)
	if left == "" || right == "" {
		return false
	}
	return left == right
}

func containsOwner(owners []string, id string) bool {
	for _, owner := range owners {
		if owner == id {
			return true
		}
	}
	return false
}

func findOwnedWorktree(root, jobID string) (string, error) {
	dir := filepath.Join(root, "worktree_ownership")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", errRetirementOwnershipMissing
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return "", readErr
		}
		var owned retention.WorktreeOwnership
		if json.Unmarshal(data, &owned) == nil && owned.Retiring && containsOwner(owned.Jobs, jobID) {
			return owned.WorktreePath, nil
		}
	}
	return "", errRetirementOwnershipMissing
}

func writeJSONRecord(path string, value map[string]any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func clearNestedThreadConfig(record map[string]any, worktree, repo string) {
	config, ok := record["threadConfig"].(map[string]any)
	if !ok {
		return
	}
	if current, _ := config["worktreePath"].(string); current != "" && !sameWorktree(current, worktree) {
		return
	}
	for _, key := range []string{"worktreePath", "snapshot", "baselineSnapshot", "head", "sandbox"} {
		delete(config, key)
	}
	if repoRoot, _ := config["repoRoot"].(string); repoRoot != "" {
		config["cwd"] = repoRoot
	} else if repo != "" {
		config["cwd"] = repo
	}
	clearSandboxArgs(config)
}

func clearSandboxArgs(record map[string]any) {
	if args, ok := record["config_args"].([]any); ok {
		filtered := args[:0]
		for _, arg := range args {
			text, _ := arg.(string)
			if !strings.HasPrefix(text, "sandbox_mode=") {
				filtered = append(filtered, arg)
			}
		}
		record["config_args"] = filtered
	}
}

func clearThreadRecords(root, worktree string) error {
	dir := filepath.Join(root, "codex_exec_mcp", "threads")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var record map[string]any
		if json.Unmarshal(data, &record) != nil || record == nil {
			continue
		}
		bound, _ := record["worktreePath"].(string)
		config, _ := record["threadConfig"].(map[string]any)
		nested, _ := config["worktreePath"].(string)
		if bound != "" && !sameWorktree(bound, worktree) {
			continue
		}
		if bound == "" && (nested == "" || !sameWorktree(nested, worktree)) {
			continue
		}
		if bound != "" && sameWorktree(bound, worktree) {
			clearThreadConfig(record)
		} else {
			clearNestedThreadConfig(record, worktree, "")
		}
		if err := writeJSONRecord(path, record); err != nil {
			return err
		}
	}
	return nil
}

func clearThreadConfig(record map[string]any) {
	repo, _ := record["repoRoot"].(string)
	for _, key := range []string{"worktreePath", "snapshot", "baselineSnapshot", "head", "sandbox"} {
		delete(record, key)
	}
	if repo != "" {
		record["cwd"] = repo
	}
	clearSandboxArgs(record)
	if config, ok := record["threadConfig"].(map[string]any); ok {
		clearNestedThreadConfig(record, "", repo)
		_ = config
	}
}
