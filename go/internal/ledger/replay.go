package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type replayGitRunner func(string, []string, time.Duration, string, []byte, map[string]string) ([]byte, error)

func PrepareJobReplay(repoRoot, worktreePath, snapshot, recoveryPath string, resolvers ...func(conflicts []string, current string) (bool, error)) (map[string]any, error) {
	var resolver func([]string, string) (bool, error)
	if len(resolvers) > 0 {
		resolver = resolvers[0]
	}
	return prepareJobReplay(repoRoot, worktreePath, snapshot, recoveryPath, resolver, runGit)
}

func prepareJobReplay(repoRoot, worktreePath, snapshot, recoveryPath string, resolver func(conflicts []string, current string) (bool, error), run replayGitRunner) (map[string]any, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	worktree, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil, err
	}
	recovery, err := filepath.Abs(recoveryPath)
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(recovery, "state.json")
	saveState := func(value map[string]any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		f, err := os.CreateTemp(recovery, ".state.json.*.tmp")
		if err != nil {
			return err
		}
		name := f.Name()
		defer os.Remove(name)
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		return replaceFile(name, statePath)
	}
	git := func(dir string, args []string, input []byte, operation string) ([]byte, error) {
		return run(dir, args, 30*time.Second, operation, input, nil)
	}
	restore := func(base string) error {
		if _, err := git(worktree, []string{"read-tree", "--reset", base}, nil, "restoring worker index"); err != nil {
			return err
		}
		if _, err := git(worktree, []string{"clean", "-fdx"}, nil, "clearing replay worktree"); err != nil {
			return err
		}
		if _, err := git(worktree, []string{"checkout-index", "--all", "--force"}, nil, "restoring worker baseline"); err != nil {
			return err
		}
		return copyReplayTree(filepath.Join(recovery, "worker-tree"), worktree)
	}
	result := func(status, tree string, paths any) map[string]any {
		return map[string]any{"status": status, "snapshot": tree, "paths": paths}
	}
	var prior map[string]any
	if _, statErr := os.Stat(recovery); statErr == nil {
		data, readErr := os.ReadFile(statePath)
		if readErr != nil || json.Unmarshal(data, &prior) != nil {
			return result("uncertain", snapshot, []string{}), nil
		}
		switch prior["state"] {
		case "PREPARED":
			paths, ok := prior["paths"]
			if !ok {
				paths = []any{}
			}
			resolved, ok := prior["resolved_conflicts"]
			if !ok {
				resolved = []any{}
			}
			return map[string]any{"status": "prepared", "snapshot": prior["snapshot"], "paths": paths, "resolved_conflicts": resolved}, nil
		case "CONFLICT", "BLOCKED":
			paths, ok := prior["conflicts"]
			if !ok {
				paths = []any{}
			}
			return result("conflict", snapshot, paths), nil
		case "RESOLVING":
			base, ok := prior["snapshot"].(string)
			if !ok {
				base = snapshot
			}
			if err := restore(base); err != nil {
				return nil, err
			}
			conflicts, ok := prior["conflicts"]
			if !ok {
				conflicts, ok = prior["paths"]
			}
			if !ok {
				conflicts = []any{}
			}
			next := make(map[string]any, len(prior)+1)
			for k, v := range prior {
				next[k] = v
			}
			next["state"], next["conflicts"] = "BLOCKED", conflicts
			if err := saveState(next); err != nil {
				return nil, err
			}
			return result("conflict", snapshot, conflicts), nil
		case "PREPARING":
		default:
			return result("uncertain", snapshot, []string{}), nil
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}

	var patch []byte
	var paths any
	var pathList []string
	if prior != nil {
		patch, err = os.ReadFile(filepath.Join(recovery, "worker.patch"))
		if err != nil {
			return nil, err
		}
		paths, _ = prior["paths"]
		pathList = replayPaths(paths)
	} else {
		text, patchErr := jobPatchWithGit(worktree, snapshot, run)
		if patchErr != nil {
			return nil, patchErr
		}
		patch = []byte(text)
	}
	if paths == nil && len(patch) > 0 {
		names, pathErr := git(worktree, []string{"apply", "--numstat", "-z", "-"}, patch, "listing replay paths")
		if pathErr != nil {
			return nil, pathErr
		}
		for _, row := range strings.Split(string(names), "\x00") {
			if row == "" {
				continue
			}
			cols := strings.SplitN(row, "\t", 3)
			if len(cols) == 3 {
				pathList = append(pathList, decodeGitPath([]byte(cols[2])))
			}
		}
		paths = pathList
	}
	if paths == nil {
		paths = []string{}
	}
	pathList = replayPaths(paths)
	current, err := checkoutSnapshotWithGit(root, pathList, run)
	if err != nil {
		return nil, err
	}
	if current == snapshot && prior == nil {
		changed, changeErr := changedCheckoutPaths(root, "HEAD", run)
		if changeErr != nil {
			return nil, changeErr
		}
		if len(changed) == 0 {
			return result("unchanged", snapshot, []string{}), nil
		}
	}
	if prior != nil {
		if err := restore(snapshot); err != nil {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(recovery, 0700); err != nil {
			return nil, err
		}
		if err := copyReplayTree(worktree, filepath.Join(recovery, "worker-tree")); err != nil {
			return nil, err
		}
		f, createErr := os.OpenFile(filepath.Join(recovery, "worker.patch"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if createErr != nil {
			return nil, createErr
		}
		_, err = f.Write(patch)
		if err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
	}
	if err := saveState(map[string]any{"state": "PREPARING", "snapshot": snapshot, "target": current, "paths": paths}); err != nil {
		return nil, err
	}
	if _, err := git(worktree, []string{"read-tree", "--reset", current}, nil, "resetting replay index"); err != nil {
		return nil, err
	}
	if _, err := git(worktree, []string{"clean", "-fdx"}, nil, "clearing replay worktree"); err != nil {
		return nil, err
	}
	if _, err := git(worktree, []string{"checkout-index", "--all", "--force"}, nil, "restoring current checkout snapshot"); err != nil {
		return nil, err
	}
	if len(patch) > 0 {
		mergeConflicts := []string{}
		newPaths := map[string]bool{}
		for _, name := range pathList {
			pathspec := ":(literal)" + name
			source, e := git(root, []string{"ls-tree", "-z", snapshot, "--", pathspec}, nil, "checking replay source path")
			if e != nil {
				return blockReplay(restore, saveState, result, snapshot, current, paths, pathList, mergeConflicts, newPaths, e)
			}
			if len(source) == 0 {
				target, e := git(root, []string{"ls-tree", "-z", current, "--", pathspec}, nil, "checking replay target path")
				if e != nil {
					return blockReplay(restore, saveState, result, snapshot, current, paths, pathList, mergeConflicts, newPaths, e)
				}
				if len(target) == 0 {
					newPaths[name] = true
				}
			}
		}
		merged := patch
		if _, e := git(worktree, []string{"apply", "--check", "--binary", "-"}, patch, "checking direct replay"); e != nil {
			merged, e = tryThreeWayJobPatch(root, worktree, pathList, patch, &mergeConflicts)
			if e != nil {
				return blockReplay(restore, saveState, result, snapshot, current, paths, pathList, mergeConflicts, newPaths, e)
			}
		}
		if merged == nil {
			conflicts := replayConflicts(mergeConflicts, pathList, newPaths)
			if resolver != nil {
				if err := saveState(map[string]any{"state": "RESOLVING", "snapshot": snapshot, "target": current, "paths": paths, "conflicts": conflicts}); err != nil {
					return nil, err
				}
				resolved, resolveErr := callReplayResolver(resolver, conflicts, current)
				if resolveErr == nil && resolved && !hasReplayMarkers(worktree, conflicts) {
					if err := saveState(map[string]any{"state": "PREPARED", "snapshot": current, "paths": paths, "resolved_conflicts": conflicts}); err != nil {
						return nil, err
					}
					return map[string]any{"status": "prepared", "snapshot": current, "paths": paths, "resolved_conflicts": conflicts}, nil
				}
			}
			if err := restore(snapshot); err != nil {
				return nil, err
			}
			if err := saveState(map[string]any{"state": "BLOCKED", "snapshot": snapshot, "target": current, "paths": paths, "conflicts": conflicts}); err != nil {
				return nil, err
			}
			return result("conflict", snapshot, conflicts), nil
		}
		if len(merged) > 0 {
			if _, err := git(worktree, []string{"apply", "--binary", "-"}, merged, "applying replay result"); err != nil {
				return nil, err
			}
		}
	}
	if err := saveState(map[string]any{"state": "PREPARED", "snapshot": current, "paths": paths}); err != nil {
		return nil, err
	}
	return result("prepared", current, paths), nil
}

func callReplayResolver(resolver func([]string, string) (bool, error), conflicts []string, current string) (resolved bool, err error) {
	defer func() {
		if recover() != nil {
			resolved, err = false, fmt.Errorf("resolver panic")
		}
	}()
	return resolver(conflicts, current)
}

func replayConflicts(conflicts, paths []string, newPaths map[string]bool) []string {
	if len(conflicts) == 0 {
		conflicts = paths
	}
	out := make([]string, 0, len(conflicts))
	for _, name := range conflicts {
		if !newPaths[name] {
			out = append(out, name)
		}
	}
	return out
}

func blockReplay(restore func(string) error, saveState func(map[string]any) error, result func(string, string, any) map[string]any, snapshot, current string, paths any, pathList, mergeConflicts []string, newPaths map[string]bool, cause error) (map[string]any, error) {
	if _, ok := cause.(*inspectionError); !ok {
		return nil, cause
	}
	if err := restore(snapshot); err != nil {
		return nil, err
	}
	conflicts := replayConflicts(mergeConflicts, pathList, newPaths)
	if err := saveState(map[string]any{"state": "BLOCKED", "snapshot": snapshot, "target": current, "paths": paths, "conflicts": conflicts}); err != nil {
		return nil, err
	}
	return result("conflict", snapshot, conflicts), nil
}

func tryThreeWayJobPatch(root, destination string, paths []string, patch []byte, conflicts *[]string) ([]byte, error) {
	parent := filepath.Join(root, ".tmp")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(parent, "purser-apply-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	tree := filepath.Join(scratch, "tree")
	if err = os.Mkdir(tree, 0700); err != nil {
		return nil, err
	}
	gitDir, err := runGit(root, []string{"rev-parse", "--absolute-git-dir"}, 30*time.Second, "locating job apply repository", nil, nil)
	if err != nil {
		return nil, err
	}
	env := map[string]string{"GIT_DIR": strings.TrimSpace(string(gitDir)), "GIT_WORK_TREE": tree, "GIT_INDEX_FILE": filepath.Join(scratch, "index")}
	run := func(args []string, input []byte, op string) ([]byte, error) {
		return runGit(tree, args, 30*time.Second, op, input, env)
	}
	if _, err = run([]string{"read-tree", "HEAD"}, nil, "initializing isolated merge index"); err != nil {
		return nil, err
	}
	if _, err = run([]string{"checkout-index", "--all", "--force"}, nil, "populating isolated merge tree"); err != nil {
		return nil, err
	}
	for _, name := range paths {
		source, target := filepath.Join(root, filepath.FromSlash(name)), filepath.Join(tree, filepath.FromSlash(name))
		if !withinPath(root, filepath.Dir(source)) || !withinPath(tree, filepath.Dir(target)) {
			return nil, &inspectionError{"GIT_FAILED", "Job patch contains an unsafe path."}
		}
		if err = copyReplayPath(source, target); err != nil {
			return nil, err
		}
	}
	args := []string{"add", "-A", "--"}
	args = append(args, literalPathspecs(paths)...)
	if _, err = run(args, nil, "staging current job target paths"); err != nil {
		return nil, err
	}
	before, err := run([]string{"write-tree"}, nil, "saving isolated merge base")
	if err != nil {
		return nil, err
	}
	_, applyErr := run([]string{"apply", "--3way", "--index", "-"}, patch, "merging job patch")
	unmerged, err := run([]string{"diff", "--name-only", "--diff-filter=U", "-z"}, nil, "checking isolated merge conflicts")
	if err != nil {
		return nil, err
	}
	conflictSet := map[string]bool{}
	for _, name := range strings.Split(string(unmerged), "\x00") {
		if name != "" {
			conflictSet[decodeGitPath([]byte(name))] = true
		}
	}
	for _, name := range paths {
		if hasReplayMarker(filepath.Join(tree, filepath.FromSlash(name))) {
			conflictSet[name] = true
		}
	}
	if len(conflictSet) != 0 {
		for name := range conflictSet {
			*conflicts = append(*conflicts, name)
		}
		sort.Strings(*conflicts)
		if destination != "" {
			for _, name := range paths {
				source, target := filepath.Join(tree, filepath.FromSlash(name)), filepath.Join(destination, filepath.FromSlash(name))
				if !withinPath(destination, filepath.Dir(target)) {
					return nil, &inspectionError{"GIT_FAILED", "Replay path traverses a worktree symlink."}
				}
				if err = copyReplayPath(source, target); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	}
	if applyErr != nil {
		return nil, applyErr
	}
	after, err := run([]string{"write-tree"}, nil, "saving merged job tree")
	if err != nil {
		return nil, err
	}
	return run([]string{"diff", "--binary", "--no-ext-diff", "--no-textconv", strings.TrimSpace(string(before)), strings.TrimSpace(string(after)), "--"}, nil, "exporting clean merged job patch")
}

func withinPath(root, candidate string) bool {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	for {
		resolved, e := filepath.EvalSymlinks(candidate)
		if e == nil {
			candidate = resolved
			break
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return false
		}
		candidate = parent
	}
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func copyReplayPath(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	_ = os.Remove(target)
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(link, target)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, info.Mode().Perm())
}
func hasReplayMarker(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, "||||||| ") || strings.HasPrefix(line, "=======") || strings.HasPrefix(line, ">>>>>>> ") {
			return true
		}
	}
	return false
}
func hasReplayMarkers(root string, conflicts []string) bool {
	for _, name := range conflicts {
		if hasReplayMarker(filepath.Join(root, filepath.FromSlash(name))) {
			return true
		}
	}
	return false
}

func replayPaths(value any) []string {
	switch paths := value.(type) {
	case []string:
		return paths
	case []any:
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			if s, ok := p.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func copyReplayTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := destination
		if rel != "." {
			target = filepath.Join(destination, rel)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
