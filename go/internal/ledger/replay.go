package ledger

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func PrepareJobReplay(repoRoot, worktreePath, snapshot, recoveryPath string) (map[string]any, error) {
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
		return runGit(dir, args, 30*time.Second, operation, input, nil)
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
		text, patchErr := JobPatch(worktree, snapshot)
		if patchErr != nil {
			return nil, patchErr
		}
		patch = []byte(text)
	}
	current, err := CheckoutSnapshot(root)
	if err != nil {
		return nil, err
	}
	if current == snapshot && prior == nil {
		return result("unchanged", snapshot, []string{}), nil
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
		if _, err := git(worktree, []string{"apply", "--check", "--binary", "-"}, patch, "checking direct replay"); err != nil {
			return nil, &inspectionError{"REPLAY_THREE_WAY_NOT_PORTED", "Three-way replay and conflict resolution are not yet ported."}
		}
		if _, err := git(worktree, []string{"apply", "--binary", "-"}, patch, "applying replay result"); err != nil {
			return nil, err
		}
	}
	if err := saveState(map[string]any{"state": "PREPARED", "snapshot": current, "paths": paths}); err != nil {
		return nil, err
	}
	return result("prepared", current, paths), nil
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
