package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var manifestClock = time.Now

func manifestBudgetError() *inspectionError {
	return &inspectionError{"MANIFEST_BUDGET_EXCEEDED", "The scoped untracked/ignored manifest exceeded its wall-clock budget."}
}

func checkManifestDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !manifestClock().Before(deadline) {
		return manifestBudgetError()
	}
	return nil
}

func hashFile(name string, deadline time.Time) (string, int, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, 1024*1024)
	lines := 0
	var last byte
	for {
		count, readErr := file.Read(buffer)
		if count > 0 {
			if err := checkManifestDeadline(deadline); err != nil {
				return "", 0, err
			}
			chunk := buffer[:count]
			_, _ = hash.Write(chunk)
			lines += bytes.Count(chunk, []byte{'\n'})
			last = chunk[count-1]
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	if err := checkManifestDeadline(deadline); err != nil {
		return "", 0, err
	}
	info, err := os.Stat(name)
	if err != nil {
		return "", 0, err
	}
	if info.Size() > 0 && last != '\n' {
		lines++
	}
	return hex.EncodeToString(hash.Sum(nil)), lines, nil
}

func fileMode(mode os.FileMode) int {
	result := int(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		result |= 04000
	}
	if mode&os.ModeSetgid != 0 {
		result |= 02000
	}
	if mode&os.ModeSticky != 0 {
		result |= 01000
	}
	return result
}

func filesystemFingerprint(name string) (map[string]any, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"kind": "missing"}, nil
	}
	if err != nil {
		return nil, err
	}
	mode := fileMode(info.Mode())
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(name)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256([]byte(target))
		return map[string]any{"kind": "symlink", "sha256": hex.EncodeToString(digest[:]), "mode": mode}, nil
	}
	if info.Mode().IsRegular() {
		digest, _, err := hashFile(name, time.Time{})
		if err != nil {
			return nil, err
		}
		return map[string]any{"kind": "file", "sha256": digest, "mode": mode}, nil
	}
	return map[string]any{"kind": "other", "mode": mode}, nil
}

func trackedSnapshots(root string, statuses []map[string]string, timeout time.Duration, extraPaths []string) (map[string]map[string]any, error) {
	paths := make(map[string]bool)
	states := make(map[string][]string)
	for _, path := range extraPaths {
		paths[path] = true
	}
	for _, entry := range statuses {
		for _, key := range []string{"path", "original_path"} {
			if path := entry[key]; path != "" {
				paths[path] = true
			}
		}
		states[entry["path"]] = append(states[entry["path"]], entry["state"])
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	index, err := indexEntries(root, ordered, timeout)
	if err != nil {
		return nil, err
	}
	result := make(map[string]map[string]any, len(ordered))
	for _, path := range ordered {
		state := append([]string{}, states[path]...)
		sort.Strings(state)
		fingerprint, err := filesystemFingerprint(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		result[path] = map[string]any{"status": state, "index": index[path], "worktree": fingerprint}
	}
	return result, nil
}

func manifestGit(root string, args []string, timeout time.Duration, operation string, deadline time.Time) ([]byte, error) {
	remaining := deadline.Sub(manifestClock())
	if remaining <= 0 {
		return nil, manifestBudgetError()
	}
	limited := remaining <= timeout
	raw, err := runGit(root, args, min(timeout, remaining), operation, nil, nil)
	if failure, ok := err.(*inspectionError); ok && failure.Code == "GIT_TIMEOUT" && limited {
		return nil, manifestBudgetError()
	}
	return raw, err
}

func manifestPaths(raw []byte, deadline time.Time) (map[string]bool, error) {
	paths := make(map[string]bool)
	for i, path := range strings.Split(string(raw), "\x00") {
		if i%256 == 0 {
			if err := checkManifestDeadline(deadline); err != nil {
				return nil, err
			}
		}
		if path != "" {
			paths[strings.TrimSuffix(decodeGitPath([]byte(path)), "/")] = true
		}
	}
	return paths, checkManifestDeadline(deadline)
}

func transientBytecode(path string) bool {
	if strings.HasSuffix(path, ".pyc") || strings.HasSuffix(path, ".pyo") {
		return true
	}
	for _, part := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
		if part == "__pycache__" {
			return true
		}
	}
	return false
}

func scopedManifest(root string, scope []string, timeout time.Duration, budgetSeconds float64) ([]map[string]any, error) {
	if math.IsNaN(budgetSeconds) || math.IsInf(budgetSeconds, 0) || budgetSeconds <= 0 {
		return nil, &inspectionError{"INVALID_MANIFEST_BUDGET", "The manifest wall-clock budget must be a positive number."}
	}
	maximumSeconds := float64(int64(^uint64(0)>>1)) / float64(time.Second)
	duration := time.Duration(math.MaxInt64)
	if budgetSeconds < maximumSeconds {
		duration = time.Duration(budgetSeconds * float64(time.Second))
	}
	deadline := manifestClock().Add(duration)
	pathspecs := literalPathspecs(scope)
	untracked, err := manifestGit(root, append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, pathspecs...), timeout, "listing scoped untracked files", deadline)
	if err != nil {
		return nil, err
	}
	ignored, err := manifestGit(root, append([]string{"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory", "-z", "--"}, pathspecs...), timeout, "listing scoped ignored files", deadline)
	if err != nil {
		return nil, err
	}
	paths, err := manifestPaths(untracked, deadline)
	if err != nil {
		return nil, err
	}
	ignoredPaths, err := manifestPaths(ignored, deadline)
	if err != nil {
		return nil, err
	}
	for path := range ignoredPaths {
		paths[path] = true
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		if !transientBytecode(path) {
			ordered = append(ordered, path)
		}
	}
	sort.Strings(ordered)
	manifest := make([]map[string]any, 0, len(ordered))
	for _, relative := range ordered {
		if err := checkManifestDeadline(deadline); err != nil {
			return nil, err
		}
		name := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(name)
		if err != nil {
			return nil, &inspectionError{"MANIFEST_IO_FAILED", "A scoped manifest file could not be fingerprinted."}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(name)
			if err != nil {
				return nil, &inspectionError{"MANIFEST_IO_FAILED", "A scoped manifest file could not be fingerprinted."}
			}
			digest := sha256.Sum256([]byte(target))
			manifest = append(manifest, map[string]any{"path": relative, "sha256": hex.EncodeToString(digest[:]), "line_count": 0, "kind": "symlink"})
		} else if info.Mode().IsRegular() {
			digest, lines, err := hashFile(name, deadline)
			if err != nil {
				if failure, ok := err.(*inspectionError); ok && failure.Code == "MANIFEST_BUDGET_EXCEEDED" {
					return nil, err
				}
				return nil, &inspectionError{"MANIFEST_IO_FAILED", "A scoped manifest file could not be fingerprinted."}
			}
			manifest = append(manifest, map[string]any{"path": relative, "sha256": digest, "line_count": lines, "kind": "file"})
		} else if info.IsDir() {
			manifest = append(manifest, map[string]any{"path": relative, "line_count": 0, "kind": "directory"})
		}
	}
	return manifest, nil
}
