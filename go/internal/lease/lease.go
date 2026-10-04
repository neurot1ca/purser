package lease

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"purser-hook/internal/contextio"
)

// Lock acquires a scope-leases lock file and returns its release function.
func Lock(path string) (func() error, error) { return contextio.LockFile(path) }

const (
	releasedLeaseRetentionSeconds = 7 * 24 * 60 * 60
	conflictRetentionSeconds      = 7 * 24 * 60 * 60
	maxConflictPaths              = 50
)

type Result struct{ Status, Code string }

// Store applies the non-admission scope lease transitions for a project root.
type Store struct{ path, lockPath string }

// New creates a scope lease store using Python's project-state layout.
func New(projectStateRoot string) *Store {
	return &Store{
		path:     filepath.Join(projectStateRoot, "scope_leases.json"),
		lockPath: filepath.Join(projectStateRoot, "scope_leases.lock"),
	}
}

// Release releases one active or queued owner and promotes eligible queued owners.
func (s *Store) Release(ownerKey string, now float64) Result {
	missing := false
	err := s.mutate(now, func(value map[string]any) bool {
		changed, absent := s.releaseOwner(value, ownerKey, now)
		missing = absent
		return changed
	})
	if err != nil {
		return Result{"UNKNOWN", "LEASE_RELEASE_UNCERTAIN"}
	}
	if missing {
		return Result{"UNKNOWN", "LEASE_OWNER_MISSING"}
	}
	return Result{"OK", "LEASE_RELEASED"}
}

func (s *Store) releaseOwner(value map[string]any, ownerKey string, now float64) (changed, missing bool) {
	owner, exists := value["owners"].(map[string]any)[ownerKey]
	if !exists {
		return false, true
	}
	record := owner.(map[string]any)
	if record["state"] != "ACTIVE" && record["state"] != "QUEUED" {
		return false, false
	}
	record["state"], record["released_at"] = "RELEASED", now
	s.promote(value, now)
	return true, false
}

// Reconcile expires owners and promotes newly unblocked queued owners.
func (s *Store) Reconcile(now float64) (bool, error) {
	changed := false
	err := s.mutate(now, func(value map[string]any) bool {
		changed = s.promote(value, now)
		return changed
	})
	return changed, err
}

func (s *Store) mutate(now float64, apply func(map[string]any) bool) error {
	release, err := Lock(s.lockPath)
	if err != nil {
		return err
	}
	defer release()
	value, err := s.read()
	if err != nil {
		return err
	}
	if apply(value) {
		return s.write(value, now)
	}
	return nil
}

func (s *Store) read() (map[string]any, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"version": json.Number("1"), "owners": map[string]any{}}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := Validate(raw); err != nil {
		return nil, err
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

var replaceLeaseFile = os.Rename

func (s *Store) write(value map[string]any, now float64) error {
	cutoff := now - releasedLeaseRetentionSeconds
	owners := value["owners"].(map[string]any)
	activeStarts := make([]float64, 0, len(owners))
	for _, raw := range owners {
		owner := raw.(map[string]any)
		if owner["state"] == "ACTIVE" {
			if acquired, ok := pythonFloat(owner["acquired_at"]); ok {
				activeStarts = append(activeStarts, acquired)
			}
		}
	}
	for key, raw := range owners {
		owner := raw.(map[string]any)
		if owner["state"] != "RELEASED" {
			continue
		}
		released, ok := pythonFloat(owner["released_at"])
		if !ok || released >= cutoff {
			continue
		}
		retain := false
		for _, started := range activeStarts {
			if started <= released {
				retain = true
				break
			}
		}
		if !retain {
			delete(owners, key)
		}
	}
	conflicts, _ := value["conflicts"].([]any)
	recent := make([]any, 0, len(conflicts))
	for _, raw := range conflicts {
		conflict := raw.(map[string]any)
		if recorded, ok := pythonFloat(conflict["recorded_at"]); ok && recorded >= now-conflictRetentionSeconds {
			recent = append(recent, conflict)
		}
	}
	sort.SliceStable(recent, func(i, j int) bool {
		left, _ := pythonFloat(recent[i].(map[string]any)["recorded_at"])
		right, _ := pythonFloat(recent[j].(map[string]any)["recorded_at"])
		return left > right
	})
	if len(recent) > maxConflictPaths {
		recent = recent[:maxConflictPaths]
	}
	value["conflicts"] = recent
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), "scope_leases.*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(encoded); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceLeaseFile(name, s.path)
}

func (s *Store) promote(value map[string]any, now float64) bool {
	owners := value["owners"].(map[string]any)
	keys := make([]string, 0, len(owners))
	for key := range owners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := false
	for _, key := range keys {
		owner := owners[key].(map[string]any)
		if owner["state"] != "QUEUED" {
			continue
		}
		blocked := false
		if owner["hot_path_queue"] == true {
			blocked = hotPathOverlap(value, owner["scope"].([]any), now)
		} else {
			for _, otherRaw := range owners {
				other := otherRaw.(map[string]any)
				if other["state"] == "ACTIVE" && scopesOverlap(owner["scope"].([]any), other["scope"].([]any)) {
					blocked = true
					break
				}
			}
		}
		if !blocked {
			owner["state"], owner["acquired_at"] = "ACTIVE", now
			delete(owner, "hot_path_queue")
			changed = true
		}
	}
	return changed
}

// Validate checks a serialized scope_leases.json record using Python's _read rules.
func Validate(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return errors.New("scope lease record is invalid")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("scope lease record is invalid")
	}
	record, ok := value.(map[string]any)
	if !ok || !pythonOne(record["version"]) {
		return errors.New("scope lease record is invalid")
	}
	owners, ok := record["owners"].(map[string]any)
	if !ok {
		return errors.New("scope lease record is invalid")
	}
	if conflicts, exists := record["conflicts"]; exists {
		items, ok := conflicts.([]any)
		if !ok {
			return errors.New("scope lease conflicts are invalid")
		}
		for _, item := range items {
			conflict, ok := item.(map[string]any)
			if !ok || nonemptyString(conflict["path"]) == false || !pythonNumber(conflict["recorded_at"]) {
				return errors.New("scope lease conflicts are invalid")
			}
		}
	}
	for _, rawOwner := range owners {
		owner, ok := rawOwner.(map[string]any)
		if !ok || !oneOf(owner["state"], "ACTIVE", "QUEUED", "RELEASED") ||
			!isString(owner["task_id"]) {
			return errors.New("scope lease owner is invalid")
		}
		scope, ok := owner["scope"].([]any)
		if !ok || (len(scope) == 0 && owner["read_only"] != true) ||
			(owner["read_only"] == true && len(scope) != 0) {
			return errors.New("scope lease owner is invalid")
		}
		for _, path := range scope {
			if !nonemptyString(path) {
				return errors.New("scope lease owner is invalid")
			}
		}
		if owner["state"] == "QUEUED" && (!isString(owner["job_id"]) || !pythonNumber(owner["queued_at"])) {
			return errors.New("scope lease owner is invalid")
		}
	}
	return nil
}

func oneOf(value any, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func isString(value any) bool { _, ok := value.(string); return ok }
func nonemptyString(value any) bool {
	s, ok := value.(string)
	return ok && s != ""
}

func pythonNumber(value any) bool {
	if _, ok := value.(bool); ok {
		return true
	}
	_, ok := value.(json.Number)
	return ok
}

func pythonOne(value any) bool {
	n, ok := pythonFloat(value)
	return ok && n == 1
}

func pythonFloat(value any) (float64, bool) {
	if f, ok := value.(float64); ok {
		return f, true
	}
	if b, ok := value.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil
}

func scopesOverlap(left, right []any) bool {
	for _, a := range left {
		for _, b := range right {
			leftPath, rightPath := a.(string), b.(string)
			if os.PathSeparator == '\\' {
				leftPath, rightPath = strings.ToLower(leftPath), strings.ToLower(rightPath)
			}
			if leftPath == rightPath || strings.HasPrefix(leftPath, rightPath+"/") || strings.HasPrefix(rightPath, leftPath+"/") {
				return true
			}
		}
	}
	return false
}

func hotPathOverlap(value map[string]any, scope []any, now float64) bool {
	owners := value["owners"].(map[string]any)
	conflicts, _ := value["conflicts"].([]any)
	for _, raw := range owners {
		other := raw.(map[string]any)
		if other["state"] != "ACTIVE" {
			continue
		}
		for _, conflictRaw := range conflicts {
			conflict := conflictRaw.(map[string]any)
			at, _ := pythonFloat(conflict["recorded_at"])
			path := conflict["path"].(string)
			if at >= now-conflictRetentionSeconds && scopeCovers(scope, path) && scopeCovers(other["scope"].([]any), path) {
				return true
			}
		}
	}
	return false
}

func scopeCovers(scope []any, path string) bool {
	for _, raw := range scope {
		entry := raw.(string)
		left, right := entry, path
		if os.PathSeparator == '\\' {
			left, right = strings.ToLower(left), strings.ToLower(right)
		}
		if right == left || strings.HasPrefix(right, strings.TrimRight(left, "/")+"/") {
			return true
		}
	}
	return false
}
