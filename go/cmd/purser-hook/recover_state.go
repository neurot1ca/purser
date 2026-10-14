package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/jobs"
	"purser-hook/internal/retention"
)

type recoveryCandidate struct {
	Kind     string   `json:"kind"`
	Path     string   `json:"path"`
	Jobs     []string `json:"jobs,omitempty"`
	Bytes    int64    `json:"bytes"`
	Reason   string   `json:"reason,omitempty"`
	Removed  bool     `json:"removed"`
	identity fs.FileInfo
}

type recoveryReport struct {
	Root           string              `json:"state_root"`
	Apply          bool                `json:"apply"`
	Candidates     []recoveryCandidate `json:"candidates"`
	ReclaimedBytes int64               `json:"reclaimed_bytes"`
	Failures       []string            `json:"failures"`
}

func recoverStateMain(args []string) int {
	flags := flag.NewFlagSet("recover-state", flag.ContinueOnError)
	apply := flags.Bool("apply", false, "remove eligible owned residue")
	root := flags.String("state-root", stateRoot(), "Purser state root")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return 2
	}
	report, err := recoverState(*root, *apply)
	if err != nil {
		report.Failures = append(report.Failures, err.Error())
	}
	writeJSON(report)
	if len(report.Failures) != 0 {
		return 1
	}
	return 0
}

func recoverState(root string, apply bool) (recoveryReport, error) {
	report := recoveryReport{Apply: apply, Candidates: []recoveryCandidate{}, Failures: []string{}}
	var err error
	root, err = filepath.Abs(root)
	report.Root = root
	if err != nil {
		return report, err
	}
	if err = recoverySafePath(root, root); err != nil {
		return report, err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return report, errors.New("state root must be an existing directory")
	}
	// Validate coordination paths before the shared lock can create/open anything.
	for _, path := range []string{filepath.Join(root, "cleanup"), filepath.Join(root, "cleanup", "sweep.lock")} {
		if err = recoverySafePath(root, path); err != nil {
			return report, err
		}
	}
	unlock, err := jobs.CleanupSweepLock(root)
	if err != nil {
		return report, err
	}
	defer unlock()
	report.Candidates, err = recoveryInventory(root)
	if err != nil {
		return report, err
	}
	if !apply {
		return report, nil
	}
	for i := range report.Candidates {
		candidate := &report.Candidates[i]
		if candidate.Reason != "" {
			continue
		}
		if err := applyRecoveryCandidate(root, candidate); err != nil {
			candidate.Reason = err.Error()
			report.Failures = append(report.Failures, candidate.Path+": "+err.Error())
			continue
		}
		if _, err := os.Lstat(candidate.Path); os.IsNotExist(err) {
			candidate.Removed = true
			report.ReclaimedBytes += candidate.Bytes
		} else {
			candidate.Reason = "absence could not be verified"
			report.Failures = append(report.Failures, candidate.Path+": "+candidate.Reason)
		}
	}
	return report, nil
}

func recoveryInventory(root string) ([]recoveryCandidate, error) {
	result := []recoveryCandidate{}
	for _, relative := range []string{"codex_exec_mcp/jobs", "async_jobs", "preparations"} {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := recoverySafePath(root, path); err != nil {
			return []recoveryCandidate{{Path: path, Reason: err.Error()}}, nil
		}
	}
	worktrees, err := jobs.ListRetirementCandidates(root, time.Now())
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, item := range worktrees {
		if seen[item.Path] {
			continue
		}
		seen[item.Path] = true
		candidate := recoveryCandidate{Kind: "worktree", Path: item.Path, Jobs: item.Jobs, Reason: item.Reason}
		if candidate.Reason == "" {
			if _, e := os.Lstat(item.Path); os.IsNotExist(e) {
				continue
			}
		}
		result = append(result, candidate)
	}
	for _, item := range retention.ListPreparationCandidates(root, float64(time.Now().UnixNano())/1e9) {
		candidate := recoveryCandidate{Kind: "preparation", Path: item.Path, Jobs: []string{item.JobID}}
		if !item.Eligible {
			candidate.Reason = "preparation is protected or ineligible"
		}
		result = append(result, candidate)
	}
	// Include unknown directories and links as retained evidence, never as authority.
	for _, base := range []string{"wt", "preparations"} {
		path := filepath.Join(root, base)
		if e := recoverySafePath(root, path); e != nil {
			result = append(result, recoveryCandidate{Kind: base, Path: path, Reason: e.Error()})
			continue
		}
		entries, e := os.ReadDir(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			path := filepath.Join(path, entry.Name())
			found := false
			for _, candidate := range result {
				if candidate.Path == path || strings.HasPrefix(candidate.Path, path+string(filepath.Separator)) {
					found = true
					break
				}
			}
			if !found && (entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || entry.Type()&os.ModeIrregular != 0) {
				result = append(result, recoveryCandidate{Kind: base, Path: path, Reason: "missing or ambiguous ownership"})
			}
		}
	}
	for i := range result {
		candidate := &result[i]
		if candidate.Reason != "" {
			continue
		}
		if err := recoveryDisposition(root, *candidate); err != nil {
			candidate.Reason = err.Error()
			continue
		}
		candidate.identity, err = os.Lstat(candidate.Path)
		if err == nil {
			candidate.Bytes, err = recoveryTree(candidate.Path, false)
		}
		if err != nil {
			candidate.Reason = err.Error()
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

var recoveryJobID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func recoveryObject(root, path string) (map[string]any, error) {
	if err := recoverySafePath(root, path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("ambiguous record")
	}
	return value, nil
}

func recoveryDisposition(root string, candidate recoveryCandidate) error {
	if err := recoverySafePath(root, candidate.Path); err != nil {
		return err
	}
	for _, id := range candidate.Jobs {
		if !recoveryJobID.MatchString(id) {
			return errors.New("invalid owner")
		}
		job, err := recoveryObject(root, filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"))
		if err != nil || job["jobId"] != id {
			return errors.New("missing or ambiguous job identity")
		}
		if path, _ := job["worktreePath"].(string); path != "" {
			if err := recoverySafePath(filepath.Join(root, "wt"), path); err != nil {
				return errors.New("escaping or linked job worktree")
			}
		}
		index, err := recoveryObject(root, filepath.Join(root, "async_jobs", id+".json"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if index != nil && index["stage"] == "FINALIZING" {
			return errors.New("FINALIZING owner")
		}
		if index != nil && (index["jobId"] != id || index["stage"] != "CONSUMED") {
			return errors.New("unresolved or ambiguous async owner")
		}
		if candidate.Kind == "preparation" {
			state, err := recoveryObject(root, filepath.Join(candidate.Path, "state.json"))
			if err != nil || (state["state"] != "PREPARED" && state["state"] != "APPLIED") {
				return errors.New("ambiguous preparation state")
			}
			eligible := false
			for _, item := range retention.ListPreparationCandidates(root, float64(time.Now().UnixNano())/1e9) {
				if item.Path == candidate.Path && item.JobID == id {
					eligible = item.Eligible
					break
				}
			}
			if !eligible {
				return errors.New("preparation no longer eligible")
			}
		}
	}
	if candidate.Kind == "worktree" {
		eligible := false
		items, err := jobs.ListRetirementCandidates(root, time.Now())
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.Path == candidate.Path && item.Reason == "" && strings.Join(item.Jobs, ",") == strings.Join(candidate.Jobs, ",") {
				eligible = true
				break
			}
		}
		if !eligible {
			return errors.New("worktree ownership or disposition changed")
		}
		if _, err := os.Lstat(filepath.Join(candidate.Path, ".git")); !os.IsNotExist(err) {
			return errors.New("registered worktree retained: repository metadata may be outside state root")
		}
	}
	// Live bindings and malformed reference records are ambiguous, regardless of age.
	for _, relative := range []string{"codex_exec_mcp/jobs", "codex_exec_mcp/threads", "sessions", "pending", "task_retries", "active_tasks"} {
		base := filepath.Join(root, filepath.FromSlash(relative))
		if err := recoverySafePath(root, base); err != nil {
			return err
		}
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) && path == base {
				return nil
			}
			if err != nil {
				return err
			}
			if err := recoverySafePath(root, path); err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".json" {
				return nil
			}
			value, err := recoveryObject(root, path)
			if err != nil {
				return err
			}
			if relative == "codex_exec_mcp/jobs" {
				for _, id := range candidate.Jobs {
					if filepath.Base(path) == id+".json" {
						return nil
					}
				}
			}
			data, _ := json.Marshal(value)
			for _, needle := range append(append([]string{}, candidate.Jobs...), candidate.Path) {
				encoded, _ := json.Marshal(needle)
				if strings.Contains(string(data), string(encoded)) {
					return errors.New("live or unresolved reference")
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func applyRecoveryCandidate(root string, candidate *recoveryCandidate) error {
	var releases []func()
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	if candidate.Kind == "worktree" {
		if err := recoverySafePath(root, filepath.Join(root, "worktree_ownership")); err != nil {
			return err
		}
		if _, err := recoveryTree(filepath.Join(root, "worktree_ownership"), false); err != nil {
			return err
		}
		unlock, err := retention.WorktreeRetirementLock(root, candidate.Path)
		if err != nil {
			return err
		}
		releases = append(releases, func() { _ = unlock() })
	}
	ids := append([]string(nil), candidate.Jobs...)
	sort.Strings(ids)
	for i, id := range ids {
		if i > 0 && ids[i-1] == id {
			continue
		}
		if !recoveryJobID.MatchString(id) {
			return errors.New("invalid owner")
		}
		jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
		// Same one-byte/flock ownership and claim locks, with bounded admission.
		for _, path := range []string{filepath.Join(jobsDir, id+".owner-lock"), filepath.Join(root, "async_jobs", id+".claim.lock")} {
			if err := recoverySafePath(root, path); err != nil {
				return err
			}
			unlock, err := contextio.LockFile(path)
			if err != nil {
				return err
			}
			releases = append(releases, func() { _ = unlock() })
		}
	}
	if err := recoveryDisposition(root, *candidate); err != nil {
		return err
	}
	info, err := os.Lstat(candidate.Path)
	if err != nil || candidate.identity == nil || !os.SameFile(info, candidate.identity) {
		return errors.New("candidate identity changed")
	}
	if _, err := recoveryTree(candidate.Path, false); err != nil {
		return err
	}
	if candidate.Kind == "worktree" {
		owned, err := retention.MarkWorktreeRetiring(root, candidate.Path)
		if err != nil {
			return err
		}
		if strings.Join(owned.Jobs, ",") != strings.Join(candidate.Jobs, ",") {
			return errors.New("owner set changed")
		}
	}
	ownedRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer ownedRoot.Close()
	relative, err := filepath.Rel(root, candidate.Path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("unsafe candidate")
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err = recoveryDisposition(root, *candidate); err != nil {
			return err
		}
		info, statErr := os.Lstat(candidate.Path)
		if statErr != nil || !os.SameFile(info, candidate.identity) {
			return errors.New("candidate identity changed")
		}
		var size int64
		if size, err = recoveryTree(candidate.Path, attempt > 0); err != nil {
			return err
		}
		if attempt == 0 {
			candidate.Bytes = size
		}
		if candidate.Kind == "preparation" {
			// Keep the state manifest as recovery authority until every other entry is gone.
			var entries []os.DirEntry
			entries, err = os.ReadDir(candidate.Path)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.Name() == "state.json" {
					continue
				}
				if err = ownedRoot.RemoveAll(filepath.Join(relative, entry.Name())); err != nil {
					break
				}
			}
			if err == nil {
				err = ownedRoot.RemoveAll(relative)
			}
		} else {
			err = ownedRoot.RemoveAll(relative)
		}
		if _, absence := os.Lstat(candidate.Path); os.IsNotExist(absence) {
			if err != nil {
				return err
			}
			if candidate.Kind == "worktree" {
				_, err = retention.UpdateWorktreeOwnership(root, candidate.Path, func(record *retention.WorktreeOwnership) { record.WorktreeGone = true })
				return err
			}
			return nil
		}
		if attempt < 2 {
			time.Sleep(time.Duration(1<<attempt) * 50 * time.Millisecond)
		}
	}
	return fmt.Errorf("removal failed; retained for retry: %v", err)
}

func recoverySafePath(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes state root")
	}
	// Inspect ancestors too: a linked state root parent is not deletion authority.
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0) {
			return errors.New("linked path retained")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func recoveryTree(root string, writable bool) (int64, error) {
	var total int64
	ownedRoot, err := os.OpenRoot(root)
	if err != nil {
		return 0, err
	}
	defer ownedRoot.Close()
	err = fs.WalkDir(ownedRoot.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := ownedRoot.Lstat(filepath.FromSlash(path))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0 {
			return errors.New("tree contains a link")
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("tree contains an ambiguous entry")
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		if writable {
			mode := info.Mode().Perm() | 0200
			if info.IsDir() {
				mode |= 0700
			}
			return ownedRoot.Chmod(filepath.FromSlash(path), mode)
		}
		return nil
	})
	return total, err
}
