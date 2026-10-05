package ledger

import (
	"purser-hook/internal/profile"
	"strings"
)

func DetectProtectedPaths(deltaOrPaths any, protectedPaths *[]string) []string {
	var paths []any
	switch value := deltaOrPaths.(type) {
	case map[string]any:
		items, ok := value["changed_paths"].([]any)
		if !ok {
			if strings, valid := value["changed_paths"].([]string); valid {
				for _, path := range strings {
					paths = append(paths, path)
				}
			}
		} else {
			paths = items
		}
	case []string:
		for _, path := range value {
			paths = append(paths, path)
		}
	case []any:
		paths = value
	default:
		return []string{}
	}
	patterns := profile.EngineProtectedPaths()
	if protectedPaths != nil {
		patterns = *protectedPaths
	}
	protected := map[string]bool{}
	for _, raw := range paths {
		path, ok := raw.(string)
		if !ok {
			continue
		}
		path = strings.ReplaceAll(path, `\`, "/")
		for strings.HasPrefix(path, "./") {
			path = strings.TrimPrefix(path, "./")
		}
		if profile.IsProtected(path, patterns) {
			protected[path] = true
		}
	}
	return sortedKeys(protected)
}
func AttributeKnownConcurrentScopes(delta map[string]any, ownScope []string, foreignOwners []map[string]any, protectedPaths *[]string, additionalPaths []string) (map[string]any, []map[string]string) {
	if delta["status"] != OK {
		copy := make(map[string]any, len(delta))
		for key, value := range delta {
			copy[key] = value
		}
		return copy, []map[string]string{}
	}
	var own []string
	if len(ownScope) > 0 {
		var err error
		own, err = normalizeScope(ownScope)
		if err != nil {
			return delta, []map[string]string{}
		}
	}
	changed, ok := stringList(delta["changed_paths"])
	if !ok {
		return delta, []map[string]string{}
	}
	candidates := map[string]bool{}
	for _, path := range changed {
		candidates[path] = true
	}
	for _, path := range additionalPaths {
		candidates[path] = true
	}
	paths := sortedKeys(candidates)
	protected := map[string]bool{}
	for _, path := range DetectProtectedPaths(paths, protectedPaths) {
		protected[path] = true
	}
	ambiguous := map[string]bool{}
	if values, valid := stringList(delta["ambiguous_paths"]); valid {
		for _, path := range values {
			ambiguous[path] = true
		}
	}
	known := []map[string]string{}
	omitted := map[string]bool{}
	for _, path := range paths {
		tasks := map[string]bool{}
		for _, owner := range foreignOwners {
			taskID, valid := owner["task_id"].(string)
			if !valid || taskID == "" {
				continue
			}
			scope, valid := stringList(owner["scope"])
			if !valid {
				continue
			}
			if pathInScope(path, scope) {
				tasks[taskID] = true
			}
		}
		inOwn := pathInScope(path, own)
		if len(tasks) == 1 && !ambiguous[path] && !protected[path] && !inOwn {
			for taskID := range tasks {
				known = append(known, map[string]string{"path": path, "task_id": taskID})
			}
			omitted[path] = true
		}
	}
	result := make(map[string]any, len(delta))
	for key, value := range delta {
		result[key] = value
	}
	filtered := make([]string, 0, len(changed))
	for _, path := range changed {
		if !omitted[path] {
			filtered = append(filtered, path)
		}
	}
	result["changed_paths"] = filtered
	result["changed_file_count"] = len(filtered)
	if counts, valid := countMap(delta["path_line_counts"]); valid {
		for path := range counts {
			if omitted[path] {
				delete(counts, path)
			}
		}
		result["path_line_counts"] = counts
		result["changed_line_count"] = sumCounts(counts)
	}
	if sources, valid := delta["sources"].(map[string]any); valid {
		filteredSources := make(map[string]any, len(sources))
		for name, raw := range sources {
			source, ok := raw.(map[string]any)
			if !ok {
				filteredSources[name] = raw
				continue
			}
			copy := make(map[string]any, len(source))
			for key, value := range source {
				copy[key] = value
			}
			paths, _ := stringList(source["paths"])
			kept := make([]string, 0, len(paths))
			for _, path := range paths {
				if !omitted[path] {
					kept = append(kept, path)
				}
			}
			counts, _ := countMap(source["path_line_counts"])
			for path := range counts {
				if omitted[path] {
					delete(counts, path)
				}
			}
			copy["paths"], copy["path_line_counts"] = kept, counts
			copy["changed_line_count"] = sumCountsForPaths(paths, counts, omitted)
			filteredSources[name] = copy
		}
		result["sources"] = filteredSources
	}
	return result, known
}
func countMap(value any) (map[string]int, bool) {
	switch counts := value.(type) {
	case map[string]int:
		copy := make(map[string]int, len(counts))
		for path, count := range counts {
			copy[path] = count
		}
		return copy, true
	case map[string]any:
		copy := make(map[string]int, len(counts))
		for path, raw := range counts {
			count, ok := raw.(int)
			if !ok {
				if number, valid := raw.(float64); valid && number == float64(int(number)) {
					count, ok = int(number), true
				}
			}
			if !ok {
				return nil, false
			}
			copy[path] = count
		}
		return copy, true
	default:
		return nil, false
	}
}
func sumCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}
func sumCountsForPaths(paths []string, counts map[string]int, omitted map[string]bool) int {
	total := 0
	for _, path := range paths {
		if !omitted[path] {
			total += counts[path]
		}
	}
	return total
}
