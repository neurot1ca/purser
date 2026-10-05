package ledger

import (
	"path/filepath"
	"strings"
	"time"
)

// SnapshotDriftPaths reports job paths changed since the reviewed tree.
func SnapshotDriftPaths(repoRoot, snapshot string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return []string{}, nil
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	current, err := CheckoutSnapshot(root)
	if err != nil {
		return nil, err
	}
	args := []string{"diff", "--name-only", "-z", "--no-renames", snapshot, current, "--"}
	args = append(args, literalPathspecs(paths)...)
	changed, err := runGit(root, args, 30*time.Second, "checking reviewed checkout drift", nil, nil)
	if err != nil {
		return nil, err
	}
	result := []string{}
	for _, path := range strings.Split(string(changed), "\x00") {
		if path != "" {
			result = append(result, decodeGitPath([]byte(path)))
		}
	}
	return result, nil
}

// ApplyJobPatch applies a patch atomically without changing the checkout index.
func ApplyJobPatch(repoRoot, patch string) map[string]any {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return map[string]any{"applied": false, "conflicts": []string{}, "applicable": []string{}}
	}
	raw := []byte(patch)
	if len(raw) == 0 {
		return map[string]any{"applied": true, "conflicts": []string{}}
	}
	patchPaths := func() []string {
		out, e := runGit(root, []string{"apply", "--numstat", "-z", "-"}, 30*time.Second, "listing job patch paths", raw, nil)
		if e != nil {
			return nil
		}
		paths := []string{}
		for _, row := range strings.Split(string(out), "\x00") {
			if row == "" {
				continue
			}
			cols := strings.SplitN(row, "\t", 3)
			if len(cols) == 3 {
				paths = append(paths, decodeGitPath([]byte(cols[2])))
			}
		}
		return paths
	}
	paths := patchPaths()
	if len(paths) == 0 {
		if _, err = runGit(root, []string{"apply", "--binary", "-"}, 30*time.Second, "applying job patch", raw, nil); err == nil {
			return map[string]any{"applied": true, "conflicts": []string{}}
		}
		return map[string]any{"applied": false, "conflicts": []string{}, "applicable": []string{}}
	}
	checkPaths := func() ([]string, []string) {
		conflicts, applicable := []string{}, []string{}
		for _, path := range paths {
			_, e := runGit(root, []string{"apply", "--check", "--binary", "--include=" + path, "-"}, 30*time.Second, "checking job patch path", raw, nil)
			if e != nil {
				conflicts = append(conflicts, path)
			} else {
				applicable = append(applicable, path)
			}
		}
		return conflicts, applicable
	}
	if _, err = runGit(root, []string{"apply", "--check", "--binary", "-"}, 30*time.Second, "checking job patch", raw, nil); err != nil {
		conflicts, applicable := checkPaths()
		mergeConflicts := []string{}
		merged, mergeErr := tryThreeWayJobPatch(root, "", paths, raw, &mergeConflicts)
		if mergeErr == nil && merged != nil {
			if len(merged) == 0 {
				return map[string]any{"applied": true, "conflicts": []string{}}
			}
			if _, e := runGit(root, []string{"apply", "--check", "--binary", "-"}, 30*time.Second, "checking merged job patch", merged, nil); e == nil {
				if _, e = runGit(root, []string{"apply", "--binary", "-"}, 30*time.Second, "applying merged job patch", merged, nil); e == nil {
					return map[string]any{"applied": true, "conflicts": []string{}}
				}
			}
		}
		return map[string]any{"applied": false, "conflicts": conflicts, "applicable": applicable}
	}
	if _, err = runGit(root, []string{"apply", "--binary", "-"}, 30*time.Second, "applying job patch", raw, nil); err == nil {
		return map[string]any{"applied": true, "conflicts": []string{}}
	}
	conflicts, applicable := checkPaths()
	return map[string]any{"applied": false, "conflicts": conflicts, "applicable": applicable}
}

// JobPatchPaths returns the paths encoded in a generated job patch.
func JobPatchPaths(repoRoot, patch string) ([]string, error) {
	if patch == "" {
		return []string{}, nil
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	out, err := runGit(root, []string{"apply", "--numstat", "-z", "-"}, 30*time.Second,
		"listing job patch paths", []byte(patch), nil)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, row := range strings.Split(string(out), "\x00") {
		if row == "" {
			continue
		}
		cols := strings.SplitN(row, "\t", 3)
		if len(cols) == 3 {
			paths = append(paths, decodeGitPath([]byte(cols[2])))
		}
	}
	return paths, nil
}
