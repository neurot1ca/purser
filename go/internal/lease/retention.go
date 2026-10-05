package lease

import (
	"errors"
	"math"
	"path"
	"sort"
	"strings"
)

func (s *Store) SetRetentionAfter(ownerKey string, deadline, now float64) Result {
	result := Result{"UNKNOWN", "LEASE_OWNER_MISSING"}
	err := s.mutate(now, func(value map[string]any) bool {
		owner, ok := value["owners"].(map[string]any)[ownerKey]
		if !ok || !finite(deadline) {
			return false
		}
		record, ok := owner.(map[string]any)
		if !ok {
			result = Result{"UNKNOWN", "LEASE_RETENTION_UNCERTAIN"}
			return false
		}
		if record["state"] != "ACTIVE" && record["state"] != "QUEUED" {
			return false
		}
		record["retention_after"] = deadline
		result = Result{"OK", "LEASE_RETENTION_SET"}
		return true
	})
	if err != nil {
		return Result{"UNKNOWN", "LEASE_RETENTION_UNCERTAIN"}
	}
	return result
}
func (s *Store) OwnerState(ownerKey string, now float64) (string, error) {
	state := "MISSING"
	err := s.mutate(now, func(value map[string]any) bool {
		changed := s.expire(value, now)
		changed = s.promote(value, now) || changed
		if owner, ok := value["owners"].(map[string]any)[ownerKey]; ok {
			record, ok := owner.(map[string]any)
			if !ok {
				return false
			}
			if next, ok := record["state"].(string); ok {
				state = next
			} else {
				state = "UNKNOWN"
			}
		}
		return changed
	})
	return state, err
}
func (s *Store) OverlappingOwners(ownerKey string, now float64) ([]map[string]any, error) {
	var result []map[string]any
	err := s.mutate(now, func(value map[string]any) bool {
		changed := s.expire(value, now)
		changed = s.promote(value, now) || changed
		owners := value["owners"].(map[string]any)
		own, ok := owners[ownerKey]
		if !ok {
			return changed
		}
		record, ok := own.(map[string]any)
		if !ok {
			return changed
		}
		start, ok := pythonFloat(record["acquired_at"])
		if !ok {
			return changed
		}
		end := now
		if raw, exists := record["released_at"]; exists {
			end, ok = pythonFloat(raw)
			if !ok {
				return changed
			}
		}
		keys, _ := value[ownerOrderKey].([]string)
		result = make([]map[string]any, 0)
		for _, key := range keys {
			if key == ownerKey {
				continue
			}
			otherRaw, exists := owners[key]
			if !exists {
				continue
			}
			other, ok := otherRaw.(map[string]any)
			if !ok {
				return changed
			}
			acquired, hasAcquired := pythonFloat(other["acquired_at"])
			released, hasReleased := pythonFloat(other["released_at"])
			if !hasReleased {
				released = now
				hasReleased = true
			}
			if hasAcquired && hasReleased && acquired < end && released > start {
				result = append(result, map[string]any{"task_id": other["task_id"], "scope": other["scope"]})
			}
		}
		return changed
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("current scope lease history is missing")
	}
	return result, nil
}
func (s *Store) RecordConflicts(paths []string, now float64) error {
	clean := make(map[string]bool)
	for _, item := range paths {
		if item != "" && !path.IsAbs(item) && !hasParentPath(item) {
			clean[item] = true
		}
	}
	if len(clean) == 0 {
		return nil
	}
	return s.mutate(now, func(value map[string]any) bool {
		recent := make(map[string]map[string]any)
		order := make([]string, 0)
		prior, _ := value["conflicts"].([]any)
		for _, raw := range prior {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			at, _ := pythonFloat(item["recorded_at"])
			if at >= now-conflictRetentionSeconds {
				key, ok := item["path"].(string)
				if !ok {
					continue
				}
				if _, exists := recent[key]; !exists {
					order = append(order, key)
				}
				recent[key] = item
			}
		}
		newPaths := make([]string, 0, len(clean))
		for item := range clean {
			newPaths = append(newPaths, item)
		}
		sort.Strings(newPaths)
		for _, item := range newPaths {
			if _, exists := recent[item]; !exists {
				order = append(order, item)
			}
			recent[item] = map[string]any{"path": item, "recorded_at": now}
		}
		conflicts := make([]any, 0, len(order))
		for _, key := range order {
			conflicts = append(conflicts, recent[key])
		}
		value["conflicts"] = conflicts
		return true
	})
}

// ExtendRetention keeps an active or queued async owner through replacement work.
func (s *Store) ExtendRetention(ownerKey string, horizon, now float64) Result {
	result := Result{"UNKNOWN", "LEASE_OWNER_MISSING"}
	err := s.mutate(now, func(value map[string]any) bool {
		owner, ok := value["owners"].(map[string]any)[ownerKey]
		if !ok {
			return false
		}
		record, ok := owner.(map[string]any)
		if !ok {
			result = Result{"UNKNOWN", "LEASE_EXTENSION_UNCERTAIN"}
			return false
		}
		if (record["state"] != "ACTIVE" && record["state"] != "QUEUED") || !finite(horizon) {
			return false
		}
		prior, ok := pythonFloat(record["retention_after"])
		if !ok {
			if _, exists := record["retention_after"]; exists {
				result = Result{"UNKNOWN", "LEASE_EXTENSION_UNCERTAIN"}
				return false
			}
			prior = 0
		}
		record["retention_after"] = math.Max(prior, horizon)
		result = Result{"OK", "LEASE_EXTENDED"}
		return true
	})
	if err != nil {
		return Result{"UNKNOWN", "LEASE_EXTENSION_UNCERTAIN"}
	}
	return result
}

// Active reconciles leases and reports active owners in persisted insertion order.
func (s *Store) Active(now float64) ([]map[string]any, error) {
	var result []map[string]any
	var resultErr error
	err := s.mutate(now, func(value map[string]any) bool {
		changed := s.expire(value, now)
		changed = s.promote(value, now) || changed
		owners := value["owners"].(map[string]any)
		keys, _ := value[ownerOrderKey].([]string)
		if keys == nil {
			keys = make([]string, 0, len(owners))
			for key := range owners {
				keys = append(keys, key)
			}
			sort.Strings(keys)
		}
		result = make([]map[string]any, 0)
		for _, key := range keys {
			raw, exists := owners[key]
			if !exists {
				continue
			}
			owner, ok := raw.(map[string]any)
			if !ok || owner["state"] != "ACTIVE" {
				continue
			}
			acquired, ok := pythonFloat(owner["acquired_at"])
			if !ok {
				resultErr = errors.New("active scope lease history is missing")
				return changed
			}
			age := math.Max(0, math.Trunc(now-acquired))
			var jobState any
			if jobID, ok := owner["job_id"].(string); ok && jobID != "" {
				jobState = s.jobState(jobID)
			}
			result = append(result, map[string]any{"task_id": owner["task_id"], "scope": owner["scope"],
				"age_seconds": age, "job_state": jobState})
		}
		return changed
	})
	if err == nil {
		err = resultErr
	}
	return result, err
}

// ReleaseTask releases every active or queued owner belonging to taskID.
func (s *Store) ReleaseTask(taskID string, now float64) ([]map[string]any, error) {
	var released []map[string]any
	err := s.mutate(now, func(value map[string]any) bool {
		owners := value["owners"].(map[string]any)
		keys, _ := value[ownerOrderKey].([]string)
		if keys == nil {
			keys = make([]string, 0, len(owners))
			for key := range owners {
				keys = append(keys, key)
			}
			sort.Strings(keys)
		}
		released = make([]map[string]any, 0)
		for _, key := range keys {
			raw, exists := owners[key]
			if !exists {
				continue
			}
			owner, ok := raw.(map[string]any)
			if !ok || (owner["state"] != "ACTIVE" && owner["state"] != "QUEUED") || owner["task_id"] != taskID {
				continue
			}
			released = append(released, map[string]any{"task_id": taskID, "scope": owner["scope"], "job_id": owner["job_id"]})
			owner["state"], owner["released_at"] = "RELEASED", now
		}
		if len(released) != 0 {
			s.promote(value, now)
		}
		return len(released) != 0
	})
	return released, err
}
func hasParentPath(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
