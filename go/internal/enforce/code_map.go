package enforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/proc"
)

const (
	codeMapVersion              = 5
	codeMapMaxFiles             = 5000
	codeMapMaxFileBytes         = 512 * 1024
	codeMapMaxTotal             = 32 * 1024 * 1024
	codeMapMaxBytes             = 256 * 1024
	codeMapMaxSeconds           = 10 * time.Second
	codeMapCompletionMaxSeconds = time.Minute
)

var startCodeMapCompletionDetached = proc.StartDetached
var codeMapPreBudget = 200 * time.Millisecond

func refreshCodeMapForPre(repo, stateRoot string) (string, string) {
	return refreshCodeMapForPreWith(repo, stateRoot, codeMapPreBudget, nil, nil, startCodeMapCompletionDetached)
}

func refreshCodeMapForPreWith(repo, stateRoot string, budget time.Duration, extractors map[string]codeMapSymbolExtractor, write func(string, []byte) error, start func([]string, []string, string) error) (string, string) {
	path, status := refreshCodeMapNative(repo, stateRoot, budget, extractors, write)
	if status == "STALE" && start != nil {
		startCodeMapCompletion(repo, stateRoot, start)
	}
	return path, status
}

func startCodeMapCompletion(repo, stateRoot string, start func([]string, []string, string) error) {
	path, err := codeMapCompletionMarker(repo, stateRoot)
	if err != nil {
		return
	}
	marker, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 5*time.Minute {
			_ = os.Remove(path)
			marker, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		}
	}
	if err != nil {
		return
	}
	_ = marker.Close()
	executable, err := os.Executable()
	if err == nil {
		err = start([]string{executable, "code-map", "--repo", repo, "--budget-seconds", "60"}, os.Environ(), repo)
	}
	if err != nil {
		_ = os.Remove(path)
	}
}

func codeMapCompletionMarker(repo, stateRoot string) (string, error) {
	root, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateRoot, "projects", contextio.ProjectKey(root), "code-map", "completion.lock"), nil
}

func parseCodeMapLine(output []byte) (string, string, error) {
	if len(output) > 4096 {
		return "", "", errors.New("oversized code map output")
	}
	line := strings.TrimSuffix(string(output), "\n")
	line = strings.TrimSuffix(line, "\r")
	if line == "" || strings.ContainsAny(line, "\r\n") || !strings.HasPrefix(line, "CODE_MAP=") {
		return "", "", errors.New("malformed code map output")
	}
	parts := strings.SplitN(strings.TrimPrefix(line, "CODE_MAP="), " STATUS=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", "", errors.New("malformed code map output")
	}
	status, suffix, hasSuffix := strings.Cut(parts[1], " ")
	if (status != "FRESH" && status != "STALE" && status != "UNAVAILABLE") || (hasSuffix && suffix != "(scoped excerpt below)") {
		return "", "", errors.New("malformed code map output")
	}
	return parts[0], status, nil
}

type codeMapFile struct {
	Path      string
	Info      fs.FileInfo
	Regular   bool
	Contained bool
}

type codeMapCacheEntry struct {
	MTimeNS int64 `json:"mtime_ns"`
	Size    int64 `json:"size"`
}

type codeMapManifest struct {
	Version      int                        `json:"version"`
	Parser       string                     `json:"parser"`
	Entries      map[string]json.RawMessage `json:"entries"`
	SkippedFiles int                        `json:"skipped_files"`
}

// codeMapInventory mirrors code_map._inventory's Git path list and bounds.
func codeMapInventory(repo string, deadline time.Time) ([]string, error) {
	if err := codeMapCheckDeadline(deadline); err != nil {
		return nil, err
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, errors.New("code map deadline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	cmd := proc.Hide(exec.CommandContext(ctx, "git", "ls-files", "-co", "--exclude-standard", "-z"))
	proc.Hide(cmd)
	cmd.Dir = repo
	data, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("code map deadline")
		}
		return nil, err
	}
	unique := make(map[string]struct{})
	for _, path := range strings.Split(string(data), "\x00") {
		if path != "" {
			unique[path] = struct{}{}
		}
	}
	if len(unique) > codeMapMaxFiles {
		return nil, errors.New("file count")
	}
	paths := make([]string, 0, len(unique))
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// codeMapFileState mirrors the lstat-first classification in code_map.refresh.
func codeMapFileState(repo, relative string) (*codeMapFile, error) {
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // git still reports deleted tracked files
	}
	if err != nil {
		return nil, err
	}
	return codeMapFileStateFromInfo(root, relative, info)
}

func codeMapFileStateFromInfo(root, relative string, info fs.FileInfo) (*codeMapFile, error) {
	state := &codeMapFile{Path: relative, Info: info, Regular: info.Mode().IsRegular()}
	if !state.Regular {
		return state, nil // Python records non-regular paths as unknown entries.
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, resolved)
	state.Contained = err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	return state, nil
}

// codeMapScanInventory applies Python's lstat, containment, and byte-limit
// decisions to the ordered Git inventory. Non-regular and escaping paths are
// retained for their unknown/unavailable entries, as in code_map.refresh.
func codeMapScanInventory(repo string, deadline time.Time) ([]codeMapFile, int, error) {
	paths, err := codeMapInventory(repo, deadline)
	if err != nil {
		return nil, 0, err
	}
	files := make([]codeMapFile, 0, len(paths))
	var total int64
	skipped := 0
	for _, relative := range paths {
		if err = codeMapCheckDeadline(deadline); err != nil {
			return nil, 0, err
		}
		state, stateErr := codeMapFileState(repo, relative)
		if stateErr != nil {
			return nil, 0, stateErr
		}
		if state == nil {
			continue
		}
		if !state.Regular || !state.Contained {
			files = append(files, *state)
			continue
		}
		oversized, limitErr := codeMapWithinLimits(state.Info.Size(), total)
		if limitErr != nil {
			return nil, 0, limitErr
		}
		total += state.Info.Size()
		if oversized {
			skipped++
			continue
		}
		files = append(files, *state)
	}
	return files, skipped, nil
}

func codeMapCacheKey(info fs.FileInfo) codeMapCacheEntry {
	return codeMapCacheEntry{MTimeNS: info.ModTime().UnixNano(), Size: info.Size()}
}

func codeMapCheckDeadline(deadline time.Time) error {
	if !time.Now().Before(deadline) {
		return errors.New("code map deadline")
	}
	return nil
}

// readCodeMapCache returns a valid reusable manifest only when its identity,
// parser, map file, lock, and deadline all pass. Invalid caches are misses.
func readCodeMapCache(directory, parser string, deadline time.Time) (*codeMapManifest, bool, error) {
	if err := codeMapCheckDeadline(deadline); err != nil {
		return nil, false, err
	}
	lock, err := os.OpenFile(filepath.Join(directory, "index.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	defer lock.Close()
	if info, statErr := lock.Stat(); statErr != nil {
		return nil, false, statErr
	} else if info.Size() == 0 {
		if _, err = lock.WriteAt([]byte("0"), 0); err != nil {
			return nil, false, err
		}
	}
	for {
		if err = contextio.TryLock(lock); err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			return nil, false, errors.New("code map lock deadline")
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
	defer contextio.Unlock(lock)
	if err = codeMapCheckDeadline(deadline); err != nil {
		return nil, false, err
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	target := filepath.Join(directory, "map.txt")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, false, err
	}
	var manifest codeMapManifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return nil, false, err
	}
	if manifest.Version != codeMapVersion || manifest.Parser != parser || manifest.Entries == nil {
		return nil, false, fmt.Errorf("code map cache identity mismatch")
	}
	if info, statErr := os.Stat(target); statErr != nil || !info.Mode().IsRegular() {
		if statErr != nil {
			return nil, false, statErr
		}
		return nil, false, errors.New("code map target is not regular")
	}
	if err = codeMapCheckDeadline(deadline); err != nil {
		return nil, false, err
	}
	return &manifest, true, nil
}

func codeMapWithinLimits(fileBytes, totalBytes int64) (skip bool, err error) {
	if totalBytes+fileBytes > codeMapMaxTotal {
		return false, errors.New("total size")
	}
	return fileBytes > codeMapMaxFileBytes, nil
}
