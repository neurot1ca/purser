package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ComputeJobDelta(worktreePath, snapshot string) map[string]any {
	if len(snapshot) < 40 || len(snapshot) > 64 {
		return deltaUnknown("JOB_SNAPSHOT_INVALID", "The job snapshot tree is unavailable.", nil, nil)
	}
	for _, char := range snapshot {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return deltaUnknown("JOB_SNAPSHOT_INVALID", "The job snapshot tree is unavailable.", nil, nil)
		}
	}
	root, err := filepath.Abs(worktreePath)
	if err != nil {
		return deltaUnknown("JOB_DELTA_INSPECTION_FAILED", "The job worktree could not be inspected.", snapshot, nil)
	}
	fail := func(err error) map[string]any {
		if failure, ok := err.(*inspectionError); ok {
			return deltaUnknown(failure.Code, failure.Message, snapshot, nil)
		}
		return deltaUnknown("JOB_DELTA_INSPECTION_FAILED", "The job worktree could not be inspected.", snapshot, nil)
	}
	kind, err := runGit(root, []string{"cat-file", "-t", snapshot}, 10*time.Second, "verifying job snapshot", nil, nil)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(decodeGitPath(kind)) != "tree" {
		return deltaUnknown("JOB_SNAPSHOT_INVALID", "The job snapshot is not a tree.", snapshot, nil)
	}
	if _, err = runGit(root, []string{"add", "-A"}, 10*time.Second, "staging isolated job changes", nil, nil); err != nil {
		return fail(err)
	}
	paths, counts, err := deltaDiff(root, []string{snapshot}, "job snapshot delta")
	if err != nil {
		return fail(err)
	}
	head, err := runGit(root, []string{"rev-parse", "--verify", "HEAD"}, 10*time.Second, "reading job HEAD", nil, nil)
	if err != nil {
		return fail(err)
	}
	ordered := sortedKeys(paths)
	lineTotal := 0
	for _, count := range counts {
		lineTotal += count
	}
	sources := make(map[string]any, len(deltaSources))
	for _, name := range deltaSources {
		if name == "staged" {
			sources[name] = map[string]any{"paths": ordered, "changed_line_count": lineTotal, "path_line_counts": counts}
		} else {
			sources[name] = map[string]any{"paths": []string{}, "changed_line_count": 0, "path_line_counts": map[string]int{}}
		}
	}
	return map[string]any{"status": "OK", "code": "DELTA_COMPUTED", "baseline_head": snapshot,
		"current_head": strings.TrimSpace(decodeGitPath(head)), "changed_paths": ordered,
		"changed_file_count": len(ordered), "changed_line_count": lineTotal, "path_line_counts": counts,
		"sources": sources, "preexisting_paths": []string{}, "ambiguous_paths": []string{}, "current_scoped_manifest": []any{}}
}
func FilterJobDelta(repoRoot string, delta map[string]any) map[string]any {
	paths, ok := delta["changed_paths"].([]string)
	if delta["status"] != "OK" || !ok || len(paths) == 0 {
		return cloneMap(delta)
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return cloneMap(delta)
	}
	filtered, err := filterJobPaths(root, paths)
	if err != nil {
		// Python raises when Git cannot filter these paths; Go conservatively keeps the delta.
		return cloneMap(delta)
	}
	keep := make(map[string]bool, len(filtered))
	for _, path := range filtered {
		keep[path] = true
	}
	result := cloneMap(delta)
	result["changed_paths"], result["changed_file_count"] = filtered, len(filtered)
	if counts, ok := delta["path_line_counts"].(map[string]int); ok {
		result["path_line_counts"], result["changed_line_count"] = filterCounts(counts, keep)
	}
	if ambiguous, ok := delta["ambiguous_paths"].([]string); ok {
		result["ambiguous_paths"] = filterPaths(ambiguous, keep)
	}
	if sources, ok := delta["sources"].(map[string]any); ok {
		filteredSources := make(map[string]any, len(sources))
		for name, raw := range sources {
			source, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			source = cloneMap(source)
			if sourcePaths, ok := source["paths"].([]string); ok {
				source["paths"] = filterPaths(sourcePaths, keep)
			}
			if counts, ok := source["path_line_counts"].(map[string]int); ok {
				source["path_line_counts"], source["changed_line_count"] = filterCounts(counts, keep)
			}
			filteredSources[name] = source
		}
		result["sources"] = filteredSources
	}
	return result
}

func filterJobPaths(root string, paths []string) ([]string, error) {
	input := []byte(strings.Join(paths, "\x00") + "\x00")
	ignoredRaw, err := runGit(root, []string{"check-ignore", "--no-index", "-z", "--stdin"}, 30*time.Second,
		"filtering ignored job patch paths", input, nil)
	ignored := make(map[string]bool)
	if err != nil {
		failure, ok := err.(*inspectionError)
		if !ok || failure.Code != "GIT_FAILED" || failure.Message != "Git failed with exit code 1 while filtering ignored job patch paths." {
			return nil, err
		}
	} else {
		for _, path := range strings.Split(string(ignoredRaw), "\x00") {
			if path != "" {
				ignored[decodeGitPath([]byte(path))] = true
			}
		}
	}
	filtered := make([]string, 0, len(paths))
	for _, path := range paths {
		norm := strings.ReplaceAll(path, `\`, "/")
		cache := strings.HasSuffix(norm, ".pyc")
		for _, part := range strings.Split(norm, "/") {
			if part == ".pytest_cache" || part == "__pycache__" || part == "node_modules" {
				cache = true
			}
		}
		if !ignored[path] && !cache {
			filtered = append(filtered, path)
		}
	}
	return filtered, nil
}

// JobPatch returns a binary patch from the immutable snapshot tree to current job state.
func JobPatch(worktreePath, snapshot string) (string, error) {
	root, err := filepath.Abs(worktreePath)
	if err != nil {
		return "", err
	}
	if _, err = runGit(root, []string{"add", "-A"}, 30*time.Second, "staging job changes for patch generation", nil, nil); err != nil {
		return "", err
	}
	names, err := runGit(root, []string{"diff", "--name-only", "-z", "--no-renames", snapshot, "--"}, 30*time.Second,
		"listing job patch paths", nil, nil)
	if err != nil {
		return "", err
	}
	paths := make([]string, 0)
	for _, path := range strings.Split(string(names), "\x00") {
		if path != "" {
			paths = append(paths, decodeGitPath([]byte(path)))
		}
	}
	if len(paths) == 0 {
		return "", nil
	}
	paths, err = filterJobPaths(root, paths)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}
	args := []string{"diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", snapshot, "--"}
	for _, path := range paths {
		args = append(args, ":(literal)"+path)
	}
	patch, err := runGit(root, args, 30*time.Second, "generating job patch", nil, nil)
	return string(patch), err
}

// CheckoutSnapshot captures tracked and untracked checkout state without touching its index.
func CheckoutSnapshot(repoRoot string) (string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	gitDirRaw, err := runGit(root, []string{"rev-parse", "--absolute-git-dir"}, 30*time.Second,
		"locating checkout snapshot index", nil, nil)
	if err != nil {
		return "", err
	}
	gitDir := strings.TrimSpace(decodeGitPath(gitDirRaw))
	index, err := os.CreateTemp(gitDir, "purser-replay-*.index")
	if err != nil {
		return "", err
	}
	indexPath := index.Name()
	if err = index.Close(); err != nil {
		_ = os.Remove(indexPath)
		return "", err
	}
	if err = os.Remove(indexPath); err != nil {
		return "", err
	}
	defer func() {
		_ = os.Remove(indexPath)
		_ = os.Remove(indexPath + ".lock")
	}()
	env := map[string]string{"GIT_INDEX_FILE": indexPath}
	if _, err = runGit(root, []string{"read-tree", "HEAD"}, 30*time.Second, "initializing checkout snapshot index", nil, env); err != nil {
		return "", err
	}
	if _, err = runGit(root, []string{"add", "-A"}, 30*time.Second, "capturing current checkout state", nil, env); err != nil {
		return "", err
	}
	tree, err := runGit(root, []string{"write-tree"}, 30*time.Second, "writing current checkout snapshot", nil, env)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(decodeGitPath(tree)), nil
}
func filterPaths(paths []string, keep map[string]bool) []string {
	result := []string{}
	for _, path := range paths {
		if keep[path] {
			result = append(result, path)
		}
	}
	return result
}
func filterCounts(counts map[string]int, keep map[string]bool) (map[string]int, int) {
	result, total := map[string]int{}, 0
	for path, count := range counts {
		if keep[path] {
			result[path] = count
			total += count
		}
	}
	return result, total
}
