package retention

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"purser-hook/internal/lease"
)

// WorktreeOwnership is the directly addressable owner set for one worktree.
type WorktreeOwnership struct {
	WorktreePath     string   `json:"worktreePath"`
	Jobs             []string `json:"jobs"`
	Threads          []string `json:"threads"`
	Retiring         bool     `json:"retiring,omitempty"`
	Claimed          bool     `json:"retirementClaimed,omitempty"`
	WorktreeGone     bool     `json:"worktreeRemoved,omitempty"`
	PreparationsGone bool     `json:"preparationsRemoved,omitempty"`
}

func worktreeOwnershipPath(root, worktree string) string {
	path, err := filepath.Abs(worktree)
	if err == nil {
		worktree = filepath.Clean(path)
	}
	if runtime.GOOS == "windows" {
		worktree = strings.ToLower(worktree)
	}
	sum := sha256.Sum256([]byte(worktree))
	return filepath.Join(root, "worktree_ownership", hex.EncodeToString(sum[:])+".json")
}

// AttachWorktree records a job/thread attachment unless retirement already owns the path.
func AttachWorktree(root, worktree, jobID, threadID string) (bool, error) {
	path := worktreeOwnershipPath(root, worktree)
	unlock, err := lease.Lock(strings.TrimSuffix(path, ".json") + ".owner-lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	owned := WorktreeOwnership{WorktreePath: worktree}
	if data, readErr := os.ReadFile(path); readErr == nil {
		if err = json.Unmarshal(data, &owned); err != nil {
			return false, err
		}
	} else if !os.IsNotExist(readErr) {
		return false, readErr
	}
	if owned.Retiring {
		return false, nil
	}
	owned.WorktreePath = worktree
	owned.Jobs = addOwner(owned.Jobs, jobID)
	owned.Threads = addOwner(owned.Threads, threadID)
	return true, writeWorktreeOwnership(path, owned)
}

// MarkWorktreeRetiring atomically prevents any later attachment to the generation.
func MarkWorktreeRetiring(root, worktree string) (WorktreeOwnership, error) {
	path := worktreeOwnershipPath(root, worktree)
	unlock, err := lease.Lock(strings.TrimSuffix(path, ".json") + ".owner-lock")
	if err != nil {
		return WorktreeOwnership{}, err
	}
	defer unlock()
	owned := WorktreeOwnership{WorktreePath: worktree, Retiring: true}
	if data, readErr := os.ReadFile(path); readErr == nil {
		if err = json.Unmarshal(data, &owned); err != nil {
			return WorktreeOwnership{}, err
		}
		owned.Retiring = true
	} else if !os.IsNotExist(readErr) {
		return WorktreeOwnership{}, readErr
	}
	err = writeWorktreeOwnership(path, owned)
	return owned, err
}

// IsWorktreeRetiring reads only the ownership record for the requested path.
func IsWorktreeRetiring(root, worktree string) (bool, error) {
	path := worktreeOwnershipPath(root, worktree)
	unlock, err := lease.Lock(strings.TrimSuffix(path, ".json") + ".owner-lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var owned WorktreeOwnership
	if err = json.Unmarshal(data, &owned); err != nil {
		return false, err
	}
	return owned.Retiring, nil
}

// UpdateWorktreeOwnership changes durable retirement progress under the owner lock.
func UpdateWorktreeOwnership(root, worktree string, update func(*WorktreeOwnership)) (WorktreeOwnership, error) {
	path := worktreeOwnershipPath(root, worktree)
	unlock, err := lease.Lock(strings.TrimSuffix(path, ".json") + ".owner-lock")
	if err != nil {
		return WorktreeOwnership{}, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return WorktreeOwnership{}, err
	}
	var owned WorktreeOwnership
	if err = json.Unmarshal(data, &owned); err != nil {
		return WorktreeOwnership{}, err
	}
	update(&owned)
	return owned, writeWorktreeOwnership(path, owned)
}

// ReadWorktreeOwnership returns the exact record for a path without creating it.
func ReadWorktreeOwnership(root, worktree string) (WorktreeOwnership, error) {
	path := worktreeOwnershipPath(root, worktree)
	data, err := os.ReadFile(path)
	if err != nil {
		return WorktreeOwnership{}, err
	}
	var owned WorktreeOwnership
	err = json.Unmarshal(data, &owned)
	return owned, err
}

// WorktreeRetirementLock serializes duplicate retirement children.
func WorktreeRetirementLock(root, worktree string) (func() error, error) {
	path := worktreeOwnershipPath(root, worktree)
	return lease.Lock(strings.TrimSuffix(path, ".json") + ".retirement-lock")
}

// DeleteWorktreeOwnership removes the record after both owned targets are gone.
func DeleteWorktreeOwnership(root, worktree string) error {
	path := worktreeOwnershipPath(root, worktree)
	unlock, err := lease.Lock(strings.TrimSuffix(path, ".json") + ".owner-lock")
	if err != nil {
		return err
	}
	defer unlock()
	return os.Remove(path)
}

func addOwner(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, current := range values {
		if current == value {
			return values
		}
	}
	values = append(values, value)
	sort.Strings(values)
	return values
}

func writeWorktreeOwnership(path string, owned WorktreeOwnership) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(owned)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".worktree-ownership-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
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
