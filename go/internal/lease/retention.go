package lease

import (
	"errors"
	"path"
	"strings"
)

func (s *Store) SetRetentionAfter(ownerKey string, deadline, now float64) Result {
	result := Result{"UNKNOWN", "LEASE_OWNER_MISSING"}
	err := s.mutate(now, func(value map[string]any) bool {
		owner, ok := value["owners"].(map[string]any)[ownerKey]
		if !ok || !finite(deadline) {
			return false
		}
		record := owner.(map[string]any)
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
			state = owner.(map[string]any)["state"].(string)
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
		record := own.(map[string]any)
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
			other := owners[key].(map[string]any)
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
		prior, _ := value["conflicts"].([]any)
		for _, raw := range prior {
			item := raw.(map[string]any)
			at, _ := pythonFloat(item["recorded_at"])
			if at >= now-conflictRetentionSeconds {
				recent[item["path"].(string)] = item
			}
		}
		for item := range clean {
			recent[item] = map[string]any{"path": item, "recorded_at": now}
		}
		conflicts := make([]any, 0, len(recent))
		for _, item := range recent {
			conflicts = append(conflicts, item)
		}
		value["conflicts"] = conflicts
		return true
	})
}
func hasParentPath(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
