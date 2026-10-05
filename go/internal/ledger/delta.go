package ledger

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

func parseNameList(raw []byte) map[string]bool {
	paths := make(map[string]bool)
	for _, value := range strings.Split(string(raw), "\x00") {
		if value != "" {
			paths[decodeGitPath([]byte(value))] = true
		}
	}
	return paths
}

func parseNumstat(raw []byte) (map[string]int, error) {
	counts := make(map[string]int)
	for _, record := range strings.Split(string(raw), "\x00") {
		if record == "" {
			continue
		}
		added, remainder, found := strings.Cut(record, "\t")
		deleted, path, foundPath := strings.Cut(remainder, "\t")
		if !found || !foundPath {
			return nil, &inspectionError{"GIT_OUTPUT_INVALID", "Git numstat output was malformed."}
		}
		name := decodeGitPath([]byte(path))
		if added == "-" || deleted == "-" {
			if counts[name] < 1 {
				counts[name] = 1
			}
			continue
		}
		add, err := strconv.Atoi(strings.TrimSpace(added))
		if err != nil {
			return nil, &inspectionError{"GIT_OUTPUT_INVALID", "Git numstat counts were malformed."}
		}
		del, err := strconv.Atoi(strings.TrimSpace(deleted))
		if err != nil {
			return nil, &inspectionError{"GIT_OUTPUT_INVALID", "Git numstat counts were malformed."}
		}
		counts[name] += add + del
	}
	return counts, nil
}

func manifestDelta(baseline, current []map[string]any) (map[string]bool, map[string]int, error) {
	before, after := make(map[string]map[string]any), make(map[string]map[string]any)
	for _, entry := range baseline {
		if path, ok := entry["path"].(string); ok {
			before[path] = entry
		}
	}
	for _, entry := range current {
		if path, ok := entry["path"].(string); ok {
			after[path] = entry
		}
	}
	paths, counts := make(map[string]bool), make(map[string]int)
	all := make(map[string]bool, len(before)+len(after))
	for path := range before {
		all[path] = true
	}
	for path := range after {
		all[path] = true
	}
	ordered := make([]string, 0, len(all))
	for path := range all {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		old, hadOld := before[path]
		new, hasNew := after[path]
		if hadOld && hasNew && old["sha256"] == new["sha256"] {
			continue
		}
		paths[path] = true
		oldLines, err := manifestLineCount(old, hadOld)
		if err != nil {
			return nil, nil, err
		}
		newLines, err := manifestLineCount(new, hasNew)
		if err != nil {
			return nil, nil, err
		}
		count := oldLines
		if newLines > count {
			count = newLines
		}
		if count < 1 {
			count = 1
		}
		counts[path] = count
	}
	return paths, counts, nil
}

func manifestLineCount(entry map[string]any, exists bool) (int, error) {
	if !exists {
		return 0, nil
	}
	value, ok := entry["line_count"]
	if !ok {
		return 0, nil
	}
	switch number := value.(type) {
	case int:
		return number, nil
	case int64:
		return int(number), nil
	case json.Number:
		integer, err := strconv.Atoi(string(number))
		if err == nil {
			return integer, nil
		}
	case bool:
		if number {
			return 1, nil
		}
		return 0, nil
	}
	return 0, &inspectionError{"BASELINE_INVALID", "Manifest line counts were invalid."}
}
