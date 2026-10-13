package retention

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"purser-hook/internal/contextio"
)

const (
	CeilingStamp = ".state-retention-check"
	ceilingLock  = ".state-retention-check.lock"
	mib          = 1024 * 1024
	gib          = 1024 * mib
	maxPrepAge   = 7 * 86400
	maxPrepCount = 128
)

var stateRootMaxBytes int64 = 16 * gib
var maxPrepBytes int64 = 2 * gib
var warningOnce sync.Once
var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var renamePluginPath = os.Rename
var removeRetainedFile = os.Remove

type policy struct {
	owner       string
	maxAgeDays  int
	maxFiles    int
	maxBytes    int64
	deleteGlobs []string
}

func bounded(owner string) policy {
	return policy{owner: owner, maxAgeDays: -1, maxFiles: -1, maxBytes: -1}
}
func limits(files int, bytes int64) policy {
	return policy{maxAgeDays: -1, maxFiles: files, maxBytes: bytes}
}
func aged(days, files int, bytes int64) policy {
	return policy{maxAgeDays: days, maxFiles: files, maxBytes: bytes}
}

func byteLimit(bytes int64) policy { return policy{maxAgeDays: -1, maxFiles: -1, maxBytes: bytes} }

// RemoveOldPrefixedEntries removes top-level temporary entries matching a name
// prefix once their modification time is older than age. Failures are retried
// by the next maintenance pass.
func RemoveOldPrefixedEntries(root string, now time.Time, age time.Duration, prefixes []string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		matched := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry.Name(), prefix) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		path := filepath.Join(root, entry.Name())
		newest, err := newestTreeMtime(path)
		if err == nil && now.Sub(newest) >= age {
			_ = os.RemoveAll(path)
		}
	}
}

func newestTreeMtime(root string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var info fs.FileInfo
		if entry.Type()&fs.ModeSymlink != 0 {
			info, err = os.Lstat(path)
		} else {
			info, err = entry.Info()
		}
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

func PrunePluginCache(executable string) {
	executable, err := filepath.Abs(executable)
	if err != nil || filepath.Base(filepath.Dir(executable)) != "bin" {
		return
	}
	versionDir := filepath.Dir(filepath.Dir(executable))
	running := filepath.Base(versionDir)
	if !validPluginVersion(running) {
		return
	}
	cache := filepath.Dir(versionDir)
	if filepath.Base(cache) != "purser" || !safeRoot(cache) {
		return
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		version := strings.TrimSuffix(strings.TrimPrefix(name, "."), ".prune")
		if name != "."+version+".prune" || !validPluginVersion(version) {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.IsDir() && !isReparseInfo(info) {
			_ = removePluginTree(filepath.Join(cache, name))
		}
	}
	previous := ""
	for _, entry := range entries {
		name := entry.Name()
		if name == running || !validPluginVersion(name) {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.IsDir() && !isReparseInfo(info) && (previous == "" || newerPluginVersion(name, previous)) {
			previous = name
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == running || name == previous || !validPluginVersion(name) {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.IsDir() && !isReparseInfo(info) {
			path := filepath.Join(cache, name)
			prunePath := filepath.Join(cache, "."+name+".prune")
			if renamePluginPath(path, prunePath) == nil {
				_ = removePluginTree(prunePath)
			}
		}
	}
}

func removePluginTree(path string) bool {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	if isReparseInfo(info) || !info.IsDir() {
		if !isReparseInfo(info) {
			_ = os.Chmod(path, info.Mode().Perm()|0200)
		}
		return os.Remove(path) == nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !removePluginTree(filepath.Join(path, entry.Name())) {
			return false
		}
	}
	_ = os.Chmod(path, info.Mode().Perm()|0700)
	return os.Remove(path) == nil
}

func validPluginVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

func newerPluginVersion(a, b string) bool {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := range left {
		if len(left[i]) != len(right[i]) {
			return len(left[i]) > len(right[i])
		}
		if left[i] != right[i] {
			return left[i] > right[i]
		}
	}
	return false
}

var registry = map[string]policy{
	"preparations":              bounded("preparations"),
	"wt":                        bounded("worktrees"),
	"marketplace":               bounded("release"),
	"telemetry":                 bounded("telemetry-distill"),
	"projects":                  byteLimit(128 * mib),
	"apply_validation":          bounded("validation-retention"),
	"task_retries":              bounded("task-retry-evidence"),
	"codex_exec_mcp":            bounded("jobs-cleanup"),
	"raw_tool_outputs":          {maxAgeDays: 7, maxFiles: -1, maxBytes: 64 * mib},
	"bundle_artifacts":          {maxAgeDays: 7, maxFiles: -1, maxBytes: 32 * mib},
	"async_jobs":                bounded("jobs-cleanup"),
	"worktree_ownership":        {owner: "worktree-retirement", maxAgeDays: 3, maxFiles: -1, maxBytes: -1},
	"pending":                   bounded("jobs-cleanup"),
	"sessions":                  limits(1024, 16*mib),
	"threads":                   aged(90, 1024, 8*mib),
	"context_io":                limits(256, 16*mib),
	"active_tasks":              limits(256, 8*mib),
	"maintenance":               byteLimit(4 * mib),
	"plans":                     aged(90, 64, 16*mib),
	"opencode":                  limits(128, 16*mib),
	"model-catalog":             {maxAgeDays: 30, maxFiles: 1, maxBytes: 32 * mib, deleteGlobs: []string{"catalog.json"}},
	"diagnostics.jsonl":         byteLimit(4 * mib),
	"codex-cli-version.json":    byteLimit(mib),
	"model-version-notice.json": byteLimit(mib),
	"plugin-version.json":       byteLimit(mib),
	"auto-compact.json":         byteLimit(mib),
	CeilingStamp:                bounded("retention"),
	ceilingLock:                 bounded("retention"),
}

var defaultPolicy = policy{maxAgeDays: 30, maxFiles: -1, maxBytes: 64 * mib}

// Warning records a best-effort state-retention health failure.
func Warning(root string) {
	warningOnce.Do(func() { _, _ = fmt.Fprintln(os.Stderr, "orchestrator telemetry degraded: STATE_RETENTION_LIMIT") })
	defer func() { _ = recover() }()
	if !safeRoot(root) {
		return
	}
	telemetryDir := filepath.Join(root, "telemetry")
	if isReparsePath(telemetryDir) {
		return
	}
	path := filepath.Join(telemetryDir, "telemetry_health.json")
	if err := os.MkdirAll(telemetryDir, 0700); err != nil || isReparsePath(path) {
		return
	}
	health := map[string]any{"schema_version": 1, "status": "OK", "failure_count": 0, "last_error_code": nil}
	if data, err := os.ReadFile(path); err == nil {
		var saved map[string]any
		if json.Unmarshal(data, &saved) == nil && saved != nil {
			health = saved
		}
	}
	count, ok := health["failure_count"].(float64)
	if !ok || count < 0 || count != float64(int64(count)) {
		count = 0
	}
	health["schema_version"], health["status"] = 1, "DEGRADED"
	health["failure_count"], health["last_error_code"] = int64(count)+1, "STATE_RETENTION_LIMIT"
	health["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(health)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".telemetry_health.*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		_ = os.Rename(tmpName, path)
	}
}

// PruneState removes only explicitly regenerable files and performs the daily root check when requested.
func PruneState(root string, now float64, checkCeiling bool) {
	defer func() { _ = recover() }()
	if !safeRoot(root) {
		return
	}
	retireArtifacts(root, now, buildArtifactReferences(root))
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if isReparsePath(path) {
			continue
		}
		p, ok := registry[entry.Name()]
		if !ok {
			p = defaultPolicy
			Warning(root)
		}
		if p.owner != "" {
			continue
		}
		if prune(path, p, now) {
			Warning(root)
		}
	}
	if checkCeiling {
		checkRootCeiling(root, now)
	}
}

// Artifact deletion uses the producer's lock and rechecks disposition inside it.
// Unknown classes and missing/ambiguous ownership are never age-based authority.
type artifactReferences struct {
	values map[string]struct{}
}

func (r *artifactReferences) add(value string) {
	if value == "" {
		return
	}
	for _, item := range []string{value, filepath.Clean(value), filepath.Base(value)} {
		if item != "." && item != "" {
			r.values[item] = struct{}{}
		}
	}
}

func addArtifactValues(value any, refs *artifactReferences) {
	switch item := value.(type) {
	case string:
		refs.add(item)
	case map[string]any:
		for _, child := range item {
			addArtifactValues(child, refs)
		}
	case []any:
		for _, child := range item {
			addArtifactValues(child, refs)
		}
	}
}

func scanArtifactReferences(root, relative string, include func(map[string]any) bool, refs *artifactReferences) {
	base := filepath.Join(root, filepath.FromSlash(relative))
	for _, path := range filesUnder(base) {
		if filepath.Ext(path) != ".json" {
			continue
		}
		value := readObject(path)
		if value == nil {
			continue
		}
		if include == nil || include(value) {
			addArtifactValues(value, refs)
		}
	}
}

func buildArtifactReferences(root string) artifactReferences {
	refs := artifactReferences{values: map[string]struct{}{}}
	scanArtifactReferences(root, "pending", nil, &refs)
	scanArtifactReferences(root, "task_retries", nil, &refs)
	scanArtifactReferences(root, "codex_exec_mcp/jobs", func(value map[string]any) bool {
		return !cleanupTerminalState(stringValue(value["state"]))
	}, &refs)
	scanArtifactReferences(root, "active_tasks", func(value map[string]any) bool {
		return value["completed"] != true
	}, &refs)
	scanArtifactReferences(root, "sessions", func(value map[string]any) bool {
		return value["live_thread"] != nil
	}, &refs)
	return refs
}

func retireArtifacts(root string, now float64, refs artifactReferences) {
	for _, path := range filesUnder(filepath.Join(root, "apply_validation")) {
		if filepath.Ext(path) != ".json" || !jobIDPattern.MatchString(strings.TrimSuffix(filepath.Base(path), ".json")) {
			continue
		}
		rel, _ := filepath.Rel(filepath.Join(root, "apply_validation"), path)
		if len(strings.Split(rel, string(filepath.Separator))) != 2 {
			continue
		}
		enqueueUnlock, err := artifactLock(root, filepath.Join(filepath.Dir(path), "enqueue.lock"))
		if err != nil {
			continue
		}
		repositoryUnlock, err := artifactLock(root, filepath.Join(filepath.Dir(path), "repository.lock"))
		if err != nil {
			_ = enqueueUnlock()
			continue
		}
		unlock, err := artifactLock(root, path+".lock")
		if err != nil {
			_ = repositoryUnlock()
			_ = enqueueUnlock()
			continue
		}
		record := readObject(path)
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if record["job_id"] == id && (record["status"] == "passed" || record["status"] == "failed" || record["status"] == "error" || record["status"] == "launch_error" || record["status"] == "timeout" || record["status"] == "interrupted") {
			log := strings.TrimSuffix(path, ".json") + ".log"
			days := 14
			if record["status"] == "passed" {
				days = 3
			}
			stamp, e := fileMtime(path)
			if e == nil && now-stamp >= float64(days)*86400 {
				retireArtifactFile(root, log, now, days)
			}
			// Retain the summary if log removal was blocked: it is retry authority.
			if !pathExists(log) {
				retireArtifactFile(root, path, now, 14)
			}
		}
		_ = unlock()
		_ = repositoryUnlock()
		_ = enqueueUnlock()
	}
	for _, indexPath := range filesUnder(filepath.Join(root, "async_jobs")) {
		id := strings.TrimSuffix(filepath.Base(indexPath), ".json")
		if filepath.Ext(indexPath) != ".json" || !jobIDPattern.MatchString(id) {
			continue
		}
		if !artifactSafePath(root, indexPath) {
			continue
		}
		unlock, err := jobsLock(indexPath)
		if err != nil {
			continue
		}
		index := readObject(indexPath)
		key := stringValue(index["call_key"])
		jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
		if !artifactSafePath(root, jobPath) {
			_ = unlock()
			continue
		}
		job := readObject(jobPath)
		if index["jobId"] == id && index["stage"] == "CONSUMED" && job["jobId"] == id && cleanupTerminalState(stringValue(job["state"])) && (job["applyState"] == "applied" || job["discarded"] == true) && jobIDPattern.MatchString(key) && !pathExists(filepath.Join(root, "pending", key+".json")) {
			bundle := filepath.Join(root, "bundle_artifacts", key+".json")
			raw := filepath.Join(root, "raw_tool_outputs", key+".json")
			if !artifactSafePath(root, bundle) || !artifactSafePath(root, raw) {
				_ = unlock()
				continue
			}
			value := readObject(bundle)
			ownedRaw := filepath.Clean(stringValue(value["raw_output_path"])) == filepath.Clean(raw)
			if runtime.GOOS == "windows" {
				ownedRaw = strings.EqualFold(filepath.Clean(stringValue(value["raw_output_path"])), filepath.Clean(raw))
			}
			if value != nil && stringValue(value["task_id"]) != "" && ownedRaw && !artifactReferenced(refs, []string{id, key, stringValue(value["task_id"]), bundle, raw}) {
				// Keep the bundle as ownership evidence until its raw output is gone.
				retireArtifactFile(root, raw, now, 7)
				if !pathExists(raw) {
					retireArtifactFile(root, bundle, now, 7)
				}
			}
		}
		_ = unlock()
	}
	for _, path := range filesUnder(filepath.Join(root, "projects")) {
		rel, _ := filepath.Rel(filepath.Join(root, "projects"), path)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 || parts[1] != "code-map" || (parts[2] != "map.txt" && parts[2] != "manifest.json") {
			continue
		}
		project := filepath.Join(root, "projects", parts[0])
		unlock, err := artifactLock(root, filepath.Join(project, "code-map", "index.lock"))
		if err != nil {
			continue
		}
		if !pathExists(filepath.Join(project, "scope_leases.json")) && !pathExists(filepath.Join(project, "active_plan.json")) && !pathExists(filepath.Join(project, "active-coder.json")) && !artifactReferenced(refs, []string{parts[0]}) {
			retireArtifactFile(root, path, now, 14)
		}
		_ = unlock()
	}
}

func artifactSafePath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if isReparsePath(current) {
			return false
		}
	}
	return safeRoot(root)
}

func artifactLock(root, path string) (func() error, error) {
	if !artifactSafePath(root, path) {
		return nil, fmt.Errorf("unsafe artifact lock")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = contextio.TryLock(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		err := contextio.Unlock(f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}, nil
}

func retireArtifactFile(root, path string, now float64, days int) bool {
	if !artifactSafePath(root, path) || !regularFile(path) {
		return false
	}
	stamp, err := fileMtime(path)
	if err != nil || now-stamp < float64(days)*86400 {
		return false
	}
	if removeRetainedFile(path) != nil || !errorsIsNotExist(path) {
		Warning(root)
		return false
	}
	return true
}

func artifactReferenced(refs artifactReferences, needles []string) bool {
	for _, needle := range needles {
		for _, item := range []string{needle, filepath.Clean(needle), filepath.Base(needle)} {
			if _, ok := refs.values[item]; ok {
				return true
			}
		}
	}
	return false
}

func prune(base string, p policy, now float64) bool {
	files := filesUnder(base)
	cutoff := float64(0)
	if p.maxAgeDays >= 0 {
		cutoff = now - float64(p.maxAgeDays)*86400
	}
	candidates := eligibleFiles(files, base, p, now)
	if p.maxAgeDays >= 0 {
		for _, path := range candidates {
			if stamp, err := fileMtime(path); err == nil && stamp < cutoff {
				if os.Remove(path) == nil || errorsIsNotExist(path) {
					files = removePath(files, path)
				}
			}
		}
	}
	candidates = eligibleFiles(files, base, p, now)
	var total int64
	for _, path := range files {
		total += fileSize(path)
	}
	count := len(files)
	for _, path := range candidates {
		if (p.maxFiles < 0 || count <= p.maxFiles) && (p.maxBytes < 0 || total <= p.maxBytes) {
			break
		}
		size := fileSize(path)
		if os.Remove(path) == nil || errorsIsNotExist(path) {
			count--
			total -= size
		}
	}
	return overLimit(filesUnder(base), p, now)
}

func errorsIsNotExist(path string) bool {
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

func removePath(paths []string, target string) []string {
	for i, path := range paths {
		if path == target {
			return append(paths[:i], paths[i+1:]...)
		}
	}
	return paths
}

func eligibleFiles(files []string, base string, p policy, now float64) []string {
	result := make([]string, 0, len(files))
	for _, path := range files {
		if len(p.deleteGlobs) == 0 || strings.HasSuffix(filepath.Base(path), ".lock") {
			continue
		}
		stamp, err := fileMtime(path)
		if err != nil || now-stamp < 600 {
			continue
		}
		relative := filepath.Base(path)
		if !samePath(base, path) {
			relative, err = filepath.Rel(base, path)
			if err != nil {
				continue
			}
			relative = filepath.ToSlash(relative)
		}
		for _, pattern := range p.deleteGlobs {
			if matched, _ := filepath.Match(pattern, relative); matched {
				result = append(result, path)
				break
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := fileMtime(result[i])
		b, _ := fileMtime(result[j])
		if a == b {
			return result[i] < result[j]
		}
		return a < b
	})
	return result
}

func overLimit(files []string, p policy, now float64) bool {
	if p.maxFiles >= 0 && len(files) > p.maxFiles {
		return true
	}
	var total int64
	for _, path := range files {
		total += fileSize(path)
	}
	if p.maxBytes >= 0 && total > p.maxBytes {
		return true
	}
	if p.maxAgeDays >= 0 {
		cutoff := now - float64(p.maxAgeDays)*86400
		for _, path := range files {
			stamp, err := fileMtime(path)
			if err != nil || stamp < cutoff {
				return true
			}
		}
	}
	return false
}

func filesUnder(base string) []string {
	info, err := os.Lstat(base)
	if err != nil || isReparseInfo(info) {
		return nil
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			return []string{base}
		}
		return nil
	}
	var result []string
	err = filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path != base && isReparsePath(path) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path != base && !entry.IsDir() {
			if fileInfo, e := entry.Info(); e == nil && fileInfo.Mode().IsRegular() {
				result = append(result, path)
			}
		}
		return nil
	})
	if err != nil {
		return result
	}
	return result
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func fileMtime(path string) (float64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return float64(info.ModTime().UnixNano()) / 1e9, nil
}

func samePath(a, b string) bool {
	left, leftErr := filepath.EvalSymlinks(a)
	right, rightErr := filepath.EvalSymlinks(b)
	if leftErr == nil && rightErr == nil {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
		}
		return filepath.Clean(left) == filepath.Clean(right)
	}
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

func rootSize(root string) int64 {
	var size int64
	for _, path := range filesUnder(root) {
		size += fileSize(path)
	}
	return size
}

func checkRootCeiling(root string, now float64) {
	if !safeRoot(root) {
		return
	}
	lockPath := filepath.Join(root, ceilingLock)
	if isReparsePath(lockPath) {
		return
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	if info, e := file.Stat(); e != nil {
		return
	} else if info.Size() == 0 {
		if _, e = file.WriteAt([]byte("0"), 0); e != nil {
			return
		}
	}
	if err = contextio.TryLock(file); err != nil {
		return
	}
	defer contextio.Unlock(file)
	stamp := filepath.Join(root, CeilingStamp)
	if isReparsePath(stamp) {
		return
	}
	if mtime, e := fileMtime(stamp); e == nil && now-mtime < 86400 {
		return
	}
	if rootSize(root) > stateRootMaxBytes {
		Warning(root)
	}
	stampFile, err := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_ = stampFile.Close()
	when := time.Unix(0, int64(now*1e9))
	_ = os.Chtimes(stamp, when, when)
}

// RetirePreparations removes completed or expired orphan replay generations.
func RetirePreparations(root string, now float64, checkBytes bool) {
	if !safeRoot(root) {
		return
	}
	base := filepath.Join(root, "preparations")
	generations := preparationGenerations(root)
	byJob := make(map[string][]string)
	for _, item := range generations {
		byJob[filepath.Base(item.owner)] = append(byJob[filepath.Base(item.owner)], item.path)
	}
	sizes := make(map[string]int64)
	if checkBytes {
		for _, item := range generations {
			sizes[item.path] = treeBytes(item.path)
		}
	}
	var eligible []eligibleGeneration
	protected := make(map[string]bool)
	for jobID, paths := range byJob {
		jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
		job := readObject(jobPath)
		for _, path := range paths {
			state, ok := preparationState(path)
			if ok && protectedPreparationState(path, state, job, now) {
				protected[jobID] = true
			}
		}
		if !jobIDPattern.MatchString(jobID) {
			protected[jobID] = true
			continue
		}
		indexPath := filepath.Join(root, "async_jobs", jobID+".json")
		index := readObject(indexPath)
		if index != nil && index["stage"] == "FINALIZING" {
			protected[jobID] = true
			continue
		}
		if pathExists(jobPath) {
			if job == nil || !cleanupTerminalState(stringValue(job["state"])) || !preparationJobEligible(job, jobPath, now) || protected[jobID] {
				protected[jobID] = true
				continue
			}
		} else {
			newest := float64(0)
			valid := true
			for _, path := range paths {
				mtime, e1 := fileMtime(path)
				if e1 != nil {
					valid = false
					break
				}
				if mtime > newest {
					newest = mtime
				}
				if stateMtime, stateErr := fileMtime(filepath.Join(path, "state.json")); stateErr == nil && stateMtime > newest {
					newest = stateMtime
				}
			}
			if !valid || now-newest < maxPrepAge || protected[jobID] {
				protected[jobID] = true
				continue
			}
		}
		for _, path := range paths {
			stamp, e1 := fileMtime(path)
			stateStamp, e2 := fileMtime(filepath.Join(path, "state.json"))
			if e1 != nil || e2 != nil {
				stamp = 0
			} else if stateStamp < stamp {
				stamp = stateStamp
			}
			eligible = append(eligible, eligibleGeneration{stamp: stamp, owner: filepath.Join(base, jobID), path: path, size: sizes[path]})
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].stamp == eligible[j].stamp {
			return eligible[i].path < eligible[j].path
		}
		return eligible[i].stamp < eligible[j].stamp
	})
	total := int64(0)
	for _, size := range sizes {
		total += size
	}
	count := len(generations)
	protectedCount := count - len(eligible)
	protectedBytes := int64(0)
	if checkBytes {
		for _, item := range eligible {
			protectedBytes += item.size
		}
		protectedBytes = total - protectedBytes
	}
	removalFailed := false
	for _, item := range eligible {
		indexPath := filepath.Join(root, "async_jobs", filepath.Base(item.owner)+".json")
		unlock, lockErr := jobsLock(indexPath)
		if lockErr != nil {
			removalFailed = true
			continue
		}
		jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", filepath.Base(item.owner)+".json")
		job := readObject(jobPath)
		index := readObject(indexPath)
		state, stateOK := preparationState(item.path)
		safe := !protected[filepath.Base(item.owner)]
		if stateOK && protectedPreparationState(item.path, state, job, now) {
			safe = false
		}
		for _, sibling := range byJob[filepath.Base(item.owner)] {
			siblingState, ok := preparationState(sibling)
			if ok && protectedPreparationState(sibling, siblingState, job, now) {
				safe = false
			}
		}
		if index != nil && index["stage"] == "FINALIZING" {
			safe = false
		}
		if pathExists(jobPath) {
			safe = safe && job != nil && cleanupTerminalState(stringValue(job["state"])) && preparationJobEligible(job, jobPath, now)
		} else {
			mtime, err := fileMtime(item.path)
			safe = safe && err == nil && now-mtime >= maxPrepAge
		}
		removed := safe && removeStateTree(item.path, base)
		if removed {
			count--
			total -= item.size
			if samePath(item.path, item.owner) {
				_ = os.Remove(item.owner)
			}
		} else {
			removalFailed = true
		}
		_ = unlock()
	}
	if protectedCount > maxPrepCount || (checkBytes && protectedBytes > maxPrepBytes) ||
		(removalFailed && (count > maxPrepCount || (checkBytes && total > maxPrepBytes))) {
		Warning(root)
	}
}

func cleanupTerminalState(state string) bool {
	return state == "SUCCEEDED" || state == "FAILED" || state == "CANCELLED" || state == "TIMED_OUT" || state == "EXPIRED" || state == "SUPERSEDED"
}

// RemovePreparationTree removes one job-owned preparation directory after validating its state-root boundary.
func RemovePreparationTree(root, path string) bool {
	return removeStateTree(path, filepath.Join(root, "preparations"))
}

func preparationJobEligible(job map[string]any, path string, now float64) bool {
	if job["applyState"] == "applied" || job["discarded"] == true {
		return true
	}
	finished, ok := job["finishedAt"].(float64)
	if !ok || finished <= 0 {
		var err error
		finished, err = fileMtime(path)
		if err != nil {
			return false
		}
	}
	return now-finished >= maxPrepAge
}

func protectedPreparationState(path, state string, job map[string]any, now float64) bool {
	if state != "PREPARING" && state != "BLOCKED" && state != "RESOLVING" && state != "CONFLICT" {
		return false
	}
	if job != nil && !cleanupTerminalState(stringValue(job["state"])) {
		return true
	}
	stamp, err := fileMtime(filepath.Join(path, "state.json"))
	return err != nil || now-stamp < maxPrepAge
}

type generation struct{ owner, path string }
type eligibleGeneration struct {
	stamp       float64
	owner, path string
	size        int64
}

func preparationGenerations(root string) []generation {
	base := filepath.Join(root, "preparations")
	if isReparsePath(base) {
		return nil
	}
	jobs, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var result []generation
	for _, entry := range jobs {
		jobDir := filepath.Join(base, entry.Name())
		if isReparsePath(jobDir) || !entry.IsDir() {
			continue
		}
		children, err := os.ReadDir(jobDir)
		if err != nil {
			continue
		}
		var revisions []string
		for _, child := range children {
			childPath := filepath.Join(jobDir, child.Name())
			if strings.HasPrefix(child.Name(), "revision-") && child.IsDir() && !isReparsePath(childPath) {
				revisions = append(revisions, childPath)
			}
		}
		if len(revisions) > 0 {
			for _, path := range revisions {
				result = append(result, generation{jobDir, path})
			}
		} else if len(children) > 0 || regularFile(filepath.Join(jobDir, "state.json")) {
			result = append(result, generation{jobDir, jobDir})
		}
	}
	return result
}

func preparationState(generation string) (string, bool) {
	manifest := readObject(filepath.Join(generation, "state.json"))
	state, ok := manifest["state"].(string)
	return state, ok
}

func treeBytes(path string) int64 {
	if isReparsePath(path) {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if current != path && isReparsePath(current) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if current != path && !entry.IsDir() {
			if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func removeStateTree(path, stateRoot string) bool {
	root, err := filepath.Abs(stateRoot)
	if err != nil || isReparsePath(root) {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	parent := root
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		if isReparsePath(parent) {
			return false
		}
	}
	if !removeNode(target) {
		return false
	}
	return true
}

func removeNode(path string) bool {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	if isReparseInfo(info) || !info.IsDir() {
		_ = os.Chmod(path, info.Mode().Perm()|0200)
		return os.Remove(path) == nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !removeNode(filepath.Join(path, entry.Name())) {
			return false
		}
	}
	_ = os.Chmod(path, info.Mode().Perm()|0700)
	return os.Remove(path) == nil
}

func isReparseInfo(info fs.FileInfo) bool {
	if info == nil {
		return true
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0
}

func safeRoot(path string) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || isReparseInfo(info) {
		return false
	}
	_, err = filepath.EvalSymlinks(absolute)
	return err == nil
}

func isReparsePath(path string) bool {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	return isReparseInfo(info)
}

func readObject(path string) map[string]any {
	if isReparsePath(path) {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	return value
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && !isReparseInfo(info)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func stringValue(value any) string { s, _ := value.(string); return s }

func jobsLock(indexPath string) (func() error, error) {
	// This matches the Python maintenance lock's shared one-byte file lock.
	lockPath := strings.TrimSuffix(indexPath, filepath.Ext(indexPath)) + ".claim.lock"
	if isReparsePath(filepath.Dir(indexPath)) || isReparsePath(lockPath) {
		return nil, fmt.Errorf("claim lock path is a reparse point")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if info, e := file.Stat(); e != nil {
		_ = file.Close()
		return nil, e
	} else if info.Size() == 0 {
		if _, e = file.WriteAt([]byte("0"), 0); e != nil {
			_ = file.Close()
			return nil, e
		}
	}
	if err = contextio.TryLock(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() error {
		unlockErr := contextio.Unlock(file)
		closeErr := file.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}
