package lease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"purser-hook/internal/contextio"
)

// Lock acquires a scope-leases lock file and returns its release function.
func Lock(path string) (func() error, error) { return contextio.LockFile(path) }

const (
	releasedLeaseRetentionSeconds = 7 * 24 * 60 * 60
	conflictRetentionSeconds      = 7 * 24 * 60 * 60
	syncLeaseFallbackSeconds      = 2 * 60 * 60
	missingJobOrphanSeconds       = 10 * 60
	maxConflictPaths              = 50
	ownerOrderKey                 = "\x00purser_owner_order"
)

type Result struct{ Status, Code string }

// AdmissionResult reports lease admission, including the blocking task when present.
type AdmissionResult struct {
	Status string `json:"status"`
	Code   string `json:"code"`
	TaskID string `json:"task_id,omitempty"`
}

// AcquireOptions mirrors the optional arguments accepted by Python ScopeLeases.acquire.
type AcquireOptions struct {
	JobID          *string
	TimeoutSeconds float64
	ReadOnly       bool
	QueueConflict  bool
	AllowOverlap   bool
	HotPathQueue   bool
}

// Store applies the non-admission scope lease transitions for a project root.
type Store struct{ path, lockPath, jobsRoot string }

// New creates a scope lease store using Python's project-state layout.
func New(projectStateRoot string) *Store {
	return &Store{
		path:     filepath.Join(projectStateRoot, "scope_leases.json"),
		lockPath: filepath.Join(projectStateRoot, "scope_leases.lock"),
		jobsRoot: filepath.Clean(filepath.Join(projectStateRoot, "..", "..", "codex_exec_mcp", "jobs")),
	}
}

// Acquire reconciles expiry and promotion, then admits or queues a scoped owner under one lock.
func (s *Store) Acquire(ownerKey, taskID string, scope []string, options AcquireOptions, now float64) AdmissionResult {
	if ownerKey == "" || taskID == "" || (len(scope) == 0 && !options.ReadOnly) ||
		(len(scope) != 0 && options.ReadOnly) {
		return AdmissionResult{"UNKNOWN", "LEASE_INVALID", ""}
	}
	result := AdmissionResult{"OK", "LEASE_ACQUIRED", ""}
	err := s.mutate(now, func(value map[string]any) bool {
		changed := s.expire(value, now)
		changed = s.promote(value, now) || changed
		owners := value["owners"].(map[string]any)
		if priorRaw, exists := owners[ownerKey]; exists {
			prior := priorRaw.(map[string]any)
			if (prior["state"] == "ACTIVE" || prior["state"] == "QUEUED") && prior["task_id"] == taskID &&
				sameScope(prior["scope"].([]any), scope) {
				result.Code = "LEASE_ACQUIRED"
				if prior["state"] == "QUEUED" {
					result.Code = "LEASE_QUEUED"
				}
				return changed
			}
			result = AdmissionResult{"UNKNOWN", "LEASE_OWNER_CONFLICT", ""}
			return changed
		}
		var jobID any
		if options.JobID != nil {
			jobID = *options.JobID
		}
		hotConflict := options.HotPathQueue && !options.ReadOnly && options.JobID != nil &&
			s.hotAdmissionConflict(value, scope, *options.JobID, now)
		var conflict map[string]any
		keys, _ := value[ownerOrderKey].([]string)
		if keys == nil {
			keys = make([]string, 0, len(owners))
			for key := range owners {
				keys = append(keys, key)
			}
			sort.Strings(keys)
		}
		for _, key := range keys {
			other := owners[key].(map[string]any)
			if (other["state"] == "ACTIVE" || other["state"] == "QUEUED") &&
				scopesOverlap(toAnyScope(scope), other["scope"].([]any)) {
				conflict = other
				break
			}
		}
		olderQueued := false
		if options.QueueConflict && !options.ReadOnly && options.JobID != nil && *options.JobID != "" {
			for _, key := range keys {
				other := owners[key].(map[string]any)
				queuedAt, ok := pythonFloat(other["queued_at"])
				if other["state"] == "QUEUED" && ok && queuedAt <= now &&
					scopesOverlap(toAnyScope(scope), other["scope"].([]any)) {
					olderQueued = true
					break
				}
			}
		}
		if conflict != nil && !olderQueued && !options.AllowOverlap &&
			!(options.QueueConflict && options.JobID != nil && *options.JobID != "" && !options.ReadOnly) {
			result = AdmissionResult{"UNKNOWN", "SCOPE_LEASE_CONFLICT", conflict["task_id"].(string)}
			return changed
		}
		state := "ACTIVE"
		if olderQueued || hotConflict || (conflict != nil && !options.AllowOverlap) {
			state, result.Code = "QUEUED", "LEASE_QUEUED"
		}
		owner := map[string]any{"task_id": taskID, "scope": toAnyScope(scope), "state": state,
			"job_id": jobID, "queued_at": now}
		if state == "ACTIVE" {
			owner["acquired_at"] = now
		} else if hotConflict {
			owner["hot_path_queue"] = true
		}
		if options.ReadOnly {
			owner["read_only"] = true
		}
		if options.JobID == nil {
			deadline := float64(syncLeaseFallbackSeconds)
			if options.TimeoutSeconds > 0 && finite(options.TimeoutSeconds) {
				deadline = options.TimeoutSeconds + 60.0
			}
			owner["deadline_at"] = now + deadline
		} else {
			timeout := options.TimeoutSeconds
			if timeout <= 0 || !finite(timeout) {
				timeout = 24 * 60 * 60
			}
			owner["retention_after"] = now + timeout + 7*24*60*60
		}
		ownerKeys, _ := value[ownerOrderKey].([]string)
		value[ownerOrderKey] = append(ownerKeys, ownerKey)
		owners[ownerKey] = owner
		return true
	})
	if err != nil {
		return AdmissionResult{"UNKNOWN", "LEASE_ADMISSION_UNCERTAIN", ""}
	}
	return result
}

func toAnyScope(scope []string) []any {
	result := make([]any, len(scope))
	for i, path := range scope {
		result[i] = path
	}
	return result
}

func sameScope(left []any, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i, path := range right {
		if left[i] != path {
			return false
		}
	}
	return true
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (s *Store) hotAdmissionConflict(value map[string]any, scope []string, jobID string, now float64) bool {
	owners := value["owners"].(map[string]any)
	conflicts, _ := value["conflicts"].([]any)
	for _, raw := range owners {
		other := raw.(map[string]any)
		if other["state"] != "ACTIVE" && other["state"] != "QUEUED" || other["job_id"] == jobID {
			continue
		}
		for _, conflictRaw := range conflicts {
			conflict := conflictRaw.(map[string]any)
			at, _ := pythonFloat(conflict["recorded_at"])
			path := conflict["path"].(string)
			if at >= now-7*24*60*60 && scopeCovers(toAnyScope(scope), path) &&
				scopeCovers(other["scope"].([]any), path) {
				return true
			}
		}
	}
	return false
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
		changed = s.expire(value, now)
		changed = s.promote(value, now) || changed
		return changed
	})
	return changed, err
}

func (s *Store) jobState(jobID string) string {
	raw, err := os.ReadFile(filepath.Join(s.jobsRoot, jobID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return "MISSING"
	}
	if err != nil {
		return "UNREADABLE"
	}
	var job map[string]any
	if !utf8.Valid(raw) || json.Unmarshal(raw, &job) != nil || job == nil || job["jobId"] != jobID {
		return "UNREADABLE"
	}
	call, ok := job["call"].(string)
	if !ok {
		return "UNREADABLE"
	}
	sum := sha256.Sum256([]byte(call))
	if fmt.Sprintf("%x", sum) != jobID {
		return "UNREADABLE"
	}
	state, ok := job["state"].(string)
	if state == "EXPIRED" || (state == "FAILED" && job["errorCode"] == "JOB_EXPIRED") {
		return "JOB_EXPIRED"
	}
	if !ok {
		return "UNREADABLE"
	}
	return state
}

func (s *Store) expire(value map[string]any, now float64) bool {
	changed := false
	for _, raw := range value["owners"].(map[string]any) {
		owner := raw.(map[string]any)
		if owner["state"] != "ACTIVE" && owner["state"] != "QUEUED" {
			continue
		}
		jobID, hasJobID := owner["job_id"]
		expired := false
		if !hasJobID || jobID == nil {
			acquired, hasAcquired := pythonFloat(owner["acquired_at"])
			deadline, hasDeadline := pythonFloat(owner["deadline_at"])
			if !hasDeadline && hasAcquired {
				deadline, hasDeadline = acquired+syncLeaseFallbackSeconds, true
			}
			expired = hasDeadline && now >= deadline
		} else if id, ok := jobID.(string); ok {
			state := s.jobState(id)
			started, hasStarted := pythonFloat(owner["acquired_at"])
			if !hasStarted {
				started, hasStarted = pythonFloat(owner["queued_at"])
			}
			orphaned := state == "MISSING" && hasStarted && now >= started+missingJobOrphanSeconds
			cutoff, hasCutoff := pythonFloat(owner["retention_after"])
			expired = orphaned || s.staleJobLease(id, owner, now) || state == "JOB_EXPIRED" ||
				(state != "UNREADABLE" && state != "QUEUED" && state != "RUNNING" && hasCutoff && now >= cutoff)
		}
		if expired {
			owner["state"], owner["released_at"] = "RELEASED", now
			changed = true
		}
	}
	return changed
}

func (s *Store) staleJobLease(jobID string, owner map[string]any, now float64) bool {
	if len(jobID) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(jobID); err != nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(s.jobsRoot, jobID+".json"))
	if err != nil {
		return false
	}
	var job map[string]any
	if !utf8.Valid(raw) || json.Unmarshal(raw, &job) != nil || job == nil || job["jobId"] != jobID {
		return false
	}
	call, ok := job["call"].(string)
	if !ok || fmt.Sprintf("%x", sha256.Sum256([]byte(call))) != jobID {
		return false
	}
	state, ok := job["state"].(string)
	if !ok || !oneOf(state, "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "EXPIRED") {
		return false
	}
	worktree, _ := job["worktreePath"].(string)
	if worktree == "" {
		return true
	}
	_, err = os.Stat(worktree)
	return errors.Is(err, os.ErrNotExist)
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
	var record struct {
		Owners json.RawMessage `json:"owners"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	value[ownerOrderKey] = jsonObjectKeys(record.Owners)
	return value, nil
}

func jsonObjectKeys(raw json.RawMessage) []string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil
	}
	keys := make([]string, 0)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		key, ok := token.(string)
		if !ok {
			return nil
		}
		keys = append(keys, key)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil
		}
	}
	return keys
}

var replaceLeaseFile = os.Rename

func (s *Store) write(value map[string]any, now float64) error {
	ownerKeys, _ := value[ownerOrderKey].([]string)
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
	ordered := make(map[string]any, len(value))
	for key, item := range value {
		if key != ownerOrderKey {
			ordered[key] = item
		}
	}
	ordered["owners"] = orderedLeaseOwners{owners: owners, keys: ownerKeys}
	encoded, err := json.Marshal(ordered)
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

type orderedLeaseOwners struct {
	owners map[string]any
	keys   []string
}

func (o orderedLeaseOwners) MarshalJSON() ([]byte, error) {
	keys := append([]string(nil), o.keys...)
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		seen[key] = true
	}
	missing := make([]string, 0)
	for key := range o.owners {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	keys = append(keys, missing...)

	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for _, key := range keys {
		owner, exists := o.owners[key]
		if !exists {
			continue
		}
		if buffer.Len() > 1 {
			buffer.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		encodedOwner, err := json.Marshal(owner)
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedKey)
		buffer.WriteByte(':')
		buffer.Write(encodedOwner)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

func (s *Store) promote(value map[string]any, now float64) bool {
	owners := value["owners"].(map[string]any)
	keys := make([]string, 0, len(owners))
	var order []string
	switch raw := value[ownerOrderKey].(type) {
	case []string:
		order = raw
	case []any:
		for _, item := range raw {
			if key, ok := item.(string); ok {
				order = append(order, key)
			}
		}
	}
	positions := make(map[string]int, len(order))
	for index, key := range order {
		positions[key] = index
	}
	earlierQueued := func(otherKey, ownerKey string, other, owner map[string]any) bool {
		otherAt, otherOK := pythonFloat(other["queued_at"])
		ownerAt, ownerOK := pythonFloat(owner["queued_at"])
		if !otherOK || !ownerOK {
			return false
		}
		if otherAt != ownerAt {
			return otherAt < ownerAt
		}
		otherOrder, otherExists := positions[otherKey]
		ownerOrder, ownerExists := positions[ownerKey]
		return otherExists && ownerExists && otherOrder < ownerOrder
	}
	for key, raw := range owners {
		if raw.(map[string]any)["state"] == "QUEUED" {
			keys = append(keys, key)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		left, _ := pythonFloat(owners[keys[i]].(map[string]any)["queued_at"])
		right, _ := pythonFloat(owners[keys[j]].(map[string]any)["queued_at"])
		if left != right {
			return left < right
		}
		leftOrder, leftExists := positions[keys[i]]
		rightOrder, rightExists := positions[keys[j]]
		if leftExists != rightExists {
			return leftExists
		}
		if leftExists && leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return keys[i] < keys[j]
	})
	changed := false
	for _, key := range keys {
		owner := owners[key].(map[string]any)
		blocked := false
		if owner["hot_path_queue"] == true {
			blocked = hotPathOverlap(value, owner["scope"].([]any), now)
		} else {
			for otherKey, otherRaw := range owners {
				other := otherRaw.(map[string]any)
				if (other["state"] == "ACTIVE" || other["state"] == "QUEUED") &&
					scopesOverlap(owner["scope"].([]any), other["scope"].([]any)) &&
					(other["state"] == "ACTIVE" || earlierQueued(otherKey, key, other, owner)) {
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

func ScopesOverlap(left, right []any) bool { return scopesOverlap(left, right) }

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
