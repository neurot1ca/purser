package ledger

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

var deltaSources = []string{"committed", "staged", "unstaged", "scoped_manifest"}

func deltaDiff(root string, transition []string, operation string) (map[string]bool, map[string]int, error) {
	args := append([]string{"diff"}, transition...)
	args = append(args, "--no-renames", "--no-ext-diff", "--no-textconv", "--name-only", "-z", "--")
	raw, err := runGit(root, args, 10*time.Second, "listing "+operation+" paths", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	paths := parseNameList(raw)
	args = append([]string{"diff"}, transition...)
	args = append(args, "--no-renames", "--no-ext-diff", "--no-textconv", "--numstat", "-z", "--")
	raw, err = runGit(root, args, 10*time.Second, "counting "+operation+" lines", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	counts, err := parseNumstat(raw)
	if err != nil {
		return nil, nil, err
	}
	for path := range paths {
		if _, ok := counts[path]; !ok {
			counts[path] = 0
		}
	}
	return paths, counts, nil
}

func validList(value any) bool {
	v := reflect.ValueOf(value)
	return v.IsValid() && v.Kind() == reflect.Slice
}

func sameJSON(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

func baselineValid(b map[string]any) bool {
	head, ok := b["head"].(string)
	_, manifestEnabled := b["manifest_enabled"].(bool)
	_, snapshots := b["tracked_snapshots"].(map[string]any)
	return b["status"] == OK && ok && head != "" && validList(b["scope"]) && manifestEnabled && validList(b["status_entries"]) && snapshots && validList(b["scoped_manifest"])
}

func deltaUnknown(code, message string, baselineHead, currentHead any) map[string]any {
	return map[string]any{"status": "UNKNOWN", "code": code, "message": message, "changed_paths": nil, "baseline_head": baselineHead, "current_head": currentHead}
}

func deltaFailure(err error, baselineHead, currentHead any) map[string]any {
	if failure, ok := err.(*inspectionError); ok {
		return deltaUnknown(failure.Code, failure.Message, baselineHead, currentHead)
	}
	return deltaUnknown("DELTA_INSPECTION_FAILED", "The delta could not be determined trustworthily.", baselineHead, currentHead)
}

func mapsList(value any) ([]map[string]any, bool) {
	list := reflect.ValueOf(value)
	if !list.IsValid() || list.Kind() != reflect.Slice {
		return nil, false
	}
	result := make([]map[string]any, list.Len())
	for i := range result {
		var ok bool
		result[i], ok = list.Index(i).Interface().(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return result, true
}

// ComputeDelta mirrors Python ledger.compute_delta. A nil scope uses baseline scope.
func ComputeDelta(repoRoot string, baseline map[string]any, scope []string) map[string]any {
	var baseHead any
	if baseline != nil {
		baseHead = baseline["head"]
	}
	if baseline == nil || !baselineValid(baseline) {
		return deltaUnknown("BASELINE_INVALID", "A trustworthy baseline was not supplied.", baseHead, nil)
	}
	head := baseline["head"].(string)
	fail := func(err error, current any) map[string]any { return deltaFailure(err, head, current) }
	requested, ok := stringList(baseline["scope"])
	if !ok {
		return deltaUnknown("INVALID_SCOPE", "Scope paths must be literal repository-relative paths.", head, nil)
	}
	if scope != nil {
		requested = scope
	}
	normalized := []string{}
	var err error
	if len(requested) > 0 {
		normalized, err = normalizeScope(requested)
		if err != nil {
			return fail(err, nil)
		}
	}
	baselineScope, _ := stringList(baseline["scope"])
	if !reflect.DeepEqual(normalized, baselineScope) {
		return deltaUnknown("SCOPE_MISMATCH", "The delta scope does not match the captured baseline scope.", head, nil)
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return fail(err, nil)
	}
	raw, err := runGit(root, []string{"rev-parse", "--verify", "HEAD"}, 10*time.Second, "reading current HEAD", nil, nil)
	if err != nil {
		return fail(err, nil)
	}
	currentHead := strings.TrimSpace(decodeGitPath(raw))
	statuses, err := statusEntries(root, 10*time.Second)
	if err != nil {
		return fail(err, currentHead)
	}
	snapshots, ok := baseline["tracked_snapshots"].(map[string]any)
	if !ok {
		return deltaUnknown("DELTA_INSPECTION_FAILED", "The delta could not be determined trustworthily.", head, currentHead)
	}
	currentSnapshots, err := trackedSnapshots(root, statuses, 10*time.Second, sortedKeys(snapshots))
	if err != nil {
		return fail(err, currentHead)
	}
	committed, committedCounts, err := deltaDiff(root, []string{head, currentHead}, "committed delta")
	if err != nil {
		return fail(err, currentHead)
	}
	staged, stagedCounts, err := deltaDiff(root, []string{"--cached"}, "staged delta")
	if err != nil {
		return fail(err, currentHead)
	}
	unstaged, unstagedCounts, err := deltaDiff(root, nil, "unstaged delta")
	if err != nil {
		return fail(err, currentHead)
	}
	unchanged, mutated := map[string]bool{}, map[string]bool{}
	for path, before := range snapshots {
		after, exists := currentSnapshots[path]
		if sameJSON(after, before) && (exists || before == nil) {
			unchanged[path] = true
		} else {
			mutated[path] = true
		}
	}
	for path := range unchanged {
		delete(staged, path)
		delete(unstaged, path)
		delete(stagedCounts, path)
		delete(unstagedCounts, path)
	}
	manifest := []map[string]any{}
	if baseline["manifest_enabled"] == true {
		manifest, err = scopedManifest(root, normalized, 10*time.Second, 3)
		if err != nil {
			return fail(err, currentHead)
		}
	}
	beforeManifest, ok := mapsList(baseline["scoped_manifest"])
	if !ok {
		return deltaUnknown("DELTA_INSPECTION_FAILED", "The delta could not be determined trustworthily.", head, currentHead)
	}
	manifestPaths, manifestCounts, err := manifestDelta(beforeManifest, manifest)
	if err != nil {
		return fail(err, currentHead)
	}
	pathsBySource := []map[string]bool{committed, staged, unstaged, manifestPaths}
	countsBySource := []map[string]int{committedCounts, stagedCounts, unstagedCounts, manifestCounts}
	all, pathLines := map[string]bool{}, map[string]int{}
	for i, paths := range pathsBySource {
		for path := range paths {
			all[path] = true
			pathLines[path] += countsBySource[i][path]
		}
	}
	changed := sortedKeys(all)
	lineTotal, sources := 0, map[string]any{}
	for _, count := range pathLines {
		lineTotal += count
	}
	for i, name := range deltaSources {
		paths, total, counts := sortedKeys(pathsBySource[i]), 0, map[string]int{}
		for _, path := range paths {
			counts[path] = countsBySource[i][path]
			total += counts[path]
		}
		sources[name] = map[string]any{"paths": paths, "changed_line_count": total, "path_line_counts": counts}
	}
	lineResult, ambiguous := map[string]any{}, []string{}
	for path, count := range pathLines {
		lineResult[path] = count
	}
	for _, path := range changed {
		if mutated[path] {
			ambiguous = append(ambiguous, path)
		}
	}
	return map[string]any{"status": "OK", "code": "DELTA_COMPUTED", "baseline_head": head, "current_head": currentHead, "changed_paths": changed, "changed_file_count": len(changed), "changed_line_count": lineTotal, "path_line_counts": lineResult, "sources": sources, "preexisting_paths": sortedKeys(snapshots), "ambiguous_paths": ambiguous, "current_scoped_manifest": manifest}
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
