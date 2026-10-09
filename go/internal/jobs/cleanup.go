package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/learning"
	"purser-hook/internal/lease"
	"purser-hook/internal/proc"
	"purser-hook/internal/retention"
)

const (
	cleanupRetention = 3 * 24 * time.Hour
	maxWorktrees     = 128
)

var maxArtifactBytes int64 = 2 * 1024 * 1024 * 1024

var cleanupTerminal = map[string]bool{"SUCCEEDED": true, "FAILED": true, "CANCELLED": true, "TIMED_OUT": true, "UNKNOWN_OR_IN_PROGRESS": true, "EXPIRED": true}

type cleanupRecord struct {
	path string
	job  map[string]any
}
type worktreeListing struct {
	command string
	paths   []string
}

// CleanupJobs performs MCP startup maintenance without requiring Python.
func CleanupJobs(stateRoot string, now time.Time, checkCeiling bool) (cleanupErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cleanupErr = fmt.Errorf("panic: %v", recovered)
		}
		if cleanupErr != nil {
			message := strings.NewReplacer("\r", " ", "\n", " ").Replace(cleanupErr.Error())
			fmt.Fprintf(os.Stderr, "Purser: job cleanup aborted: %s\n", message)
		}
	}()
	recovered, err := RecoverPending(stateRoot, now)
	if err != nil {
		return fmt.Errorf("recover pending jobs: %w", err)
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return fmt.Errorf("resolve state root: %w", err)
	}
	if err = recoverRetiringWorktrees(root); err != nil {
		return fmt.Errorf("recover retiring worktrees: %w", err)
	}
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read jobs directory: %w", err)
	}
	current := float64(now.UnixNano()) / 1e9
	loaded := make([]cleanupRecord, 0)
	protected := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(jobsDir, e.Name())
		j := readCleanupJSON(p)
		if j == nil {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		if j["jobId"] == stem {
			loaded = append(loaded, cleanupRecord{p, j})
		}
		state, _ := j["state"].(string)
		if state == "UNKNOWN_OR_IN_PROGRESS" || !cleanupTerminal[state] || cleanupConflictProtected(root, j, state, current, p) {
			if k := worktreeKey(j["worktreePath"]); k != "" {
				protected[k] = true
			}
		}
	}
	owners := map[string]map[string]any{}
	for _, r := range loaded {
		if k := worktreeKey(r.job["worktreePath"]); k != "" && owners[k] == nil {
			owners[k] = r.job
		}
	}
	for _, r := range latestThreadJobs(loaded) {
		if k := worktreeKey(r.job["worktreePath"]); k != "" {
			protected[k] = true
		}
	}
	// The hard byte ceiling may reclaim old terminal artifacts, including latest-thread
	// artifacts; nonterminal and recent unapplied jobs remain protected.
	type removal struct {
		cleanupRecord
		expired  bool
		finished bool
		stamp    float64
	}
	var removals, candidates []removal
	for _, r := range loaded {
		j := r.job
		state, _ := j["state"].(string)
		if !cleanupTerminal[state] {
			continue
		}
		k := worktreeKey(j["worktreePath"])
		removeWT := j["worktreePath"] != nil && cleanupEligible(j, state, current, r.path) && !protected[k]
		launch := state != "UNKNOWN_OR_IN_PROGRESS" && regularFile(filepath.Join(jobsDir, strings.TrimSuffix(filepath.Base(r.path), ".json")+".launch"))
		stamp, valid := cleanupNumber(j["finishedAt"])
		finished := valid
		if !valid {
			if fi, e := os.Stat(r.path); e == nil {
				stamp = float64(fi.ModTime().UnixNano()) / 1e9
			} else {
				stamp = current
			}
		}
		expired := current-stamp >= cleanupRetention.Seconds()
		if removeWT || launch || expired {
			x := removal{r, expired, finished, stamp}
			removals = append(removals, x)
			if expired && (j["worktreePath"] == nil || (cleanupEligible(j, state, current, r.path) && !protected[k])) {
				candidates = append(candidates, x)
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.finished != b.finished {
			return a.finished
		}
		if a.stamp != b.stamp {
			return a.stamp < b.stamp
		}
		return filepath.Base(a.path) < filepath.Base(b.path)
	})
	distilled := map[string]bool{}
	if len(removals) > 0 {
		if checkCeiling {
			src := make([]learning.Source, 0, len(candidates))
			existing := map[string]bool{}
			for _, r := range loaded {
				existing[learning.SourceKey(strings.TrimSuffix(filepath.Base(r.path), ".json"))] = true
			}
			for _, r := range candidates {
				src = append(src, learning.Source{Path: r.path, Job: r.job})
			}
			if d, e := learning.DistillJobs(root, src, current, learning.MaxDistillPerRun, existing); e == nil {
				distilled = d
			}
		} else {
			summary := learning.ReadSummary(root)
			if sources, ok := summary["sources"].([]any); ok {
				for _, v := range sources {
					if s, ok := v.(string); ok {
						distilled[s] = true
					}
				}
			}
		}
		undistilled := 0
		for _, r := range candidates {
			if !distilled[learning.SourceKey(strings.TrimSuffix(filepath.Base(r.path), ".json"))] {
				undistilled++
			}
		}
		if undistilled > maxJobRecords {
			retention.Warning(root)
		}
	}
	listings := map[string]*worktreeListing{}
	prune := map[string]string{}
	actions := recovered
	for _, r := range removals {
		if actions >= maxJobRecords {
			break
		}
		j := r.job
		state, _ := j["state"].(string)
		if j["worktreePath"] != nil && !cleanupEligible(j, state, current, r.path) {
			continue
		}
		acted := false
		wt, _ := j["worktreePath"].(string)
		if wt != "" && cleanupEligible(j, state, current, r.path) && !protected[worktreeKey(wt)] {
			if repo, ok := j["repoRoot"].(string); ok && repo != "" {
				acted = removeWorktree(root, repo, wt, listings, prune) || acted
			}
		}
		stem := strings.TrimSuffix(filepath.Base(r.path), ".json")
		if state != "UNKNOWN_OR_IN_PROGRESS" {
			p := filepath.Join(jobsDir, stem+".launch")
			if removeSafe(root, p) {
				acted = true
			}
		}
		if !r.expired || !distilled[learning.SourceKey(stem)] {
			if acted {
				actions++
			}
			continue
		}
		// Remove only job-owned files under the state root; every component is checked for links.
		indexPath := filepath.Join(root, "async_jobs", stem+".json")
		claimPath := strings.TrimSuffix(indexPath, filepath.Ext(indexPath)) + ".claim.lock"
		if !cleanupPath(root, claimPath) {
			continue
		}
		unlock, lockErr := contextio.LockFile(claimPath)
		if lockErr != nil {
			continue
		}
		if pathExists(indexPath) && !regularFile(indexPath) {
			_ = unlock()
			continue
		}
		idx := readCleanupJSON(indexPath)
		if regularFile(indexPath) {
			callKey, keyOK := idx["call_key"].(string)
			project, projectOK := idx["project_key"].(string)
			if idx == nil || idx["jobId"] != stem || idx["call"] != j["call"] || !keyOK || !validJobID(callKey) || !projectOK || !validProjectKey(project) || idx["stage"] == "FINALIZING" {
				_ = unlock()
				continue
			}
		}
		callKey, _ := idx["call_key"].(string)
		pendingPath := ""
		if callKey != "" {
			pendingPath = filepath.Join(root, "pending", callKey+".json")
		}
		pending := readCleanupJSON(pendingPath)
		if pathExists(pendingPath) && (pending == nil || pending["job_id"] != stem || pending["async_call"] != idx["call"]) {
			_ = unlock()
			continue
		}
		if idx != nil {
			var owner any = callKey
			if pending != nil {
				owner = pending["lease_owner"]
			}
			if ownerString, ok := owner.(string); ok && ownerString != "" {
				project, _ := idx["project_key"].(string)
				released := lease.New(filepath.Join(root, "projects", project)).Release(ownerString, current)
				if released.Status != "OK" && released.Code != "LEASE_OWNER_MISSING" {
					_ = unlock()
					continue
				}
			}
		}
		if !acted {
			actions++
		}
		pendingExists := pathExists(pendingPath)
		if pendingExists && idx != nil && idx["stage"] != "CONSUMED" && idx["unfetched_event_emitted"] != true {
			idx["unfetched_event_emitted"] = true
			data, marshalErr := json.Marshal(idx)
			if marshalErr != nil || atomicWrite(indexPath, data) != nil {
				_ = unlock()
				continue
			}
			enforce.RecordUnfetchedResult(root, idx["project_key"], pending["route"])
		}
		if name, ok := j["outputFile"].(string); ok && name == stem+".output" {
			removeSafe(root, filepath.Join(jobsDir, name))
		}
		if idx != nil {
			if key, ok := idx["call_key"].(string); ok && validJobID(key) {
				for _, p := range []string{filepath.Join(root, "raw_tool_outputs", key+".json"), filepath.Join(root, "raw_tool_outputs", key+"-failure.json"), filepath.Join(root, "bundle_artifacts", key+".json")} {
					removeSafe(root, p)
				}
			}
		}
		removeSafe(root, r.path)
		removeSafe(root, filepath.Join(jobsDir, stem+".launch"))
		if pendingPath != "" {
			removeSafe(root, pendingPath)
		}
		if idx != nil {
			removeSafe(root, indexPath)
		}
		_ = unlock()
	}
	cleanupOrphanWorktrees(root, current, protected, listings, prune, owners)
	enforceWorktreeCount(root, current, protected, listings, prune, owners)
	byteCheck := false
	if checkCeiling {
		if fi, e := os.Stat(filepath.Join(root, retention.CeilingStamp)); e != nil {
			byteCheck = true
		} else {
			byteCheck = current-float64(fi.ModTime().UnixNano())/1e9 >= 86400
		}
	}
	retention.RetirePreparations(root, current, byteCheck)
	enforceArtifactSizeLimit(root, current, loaded, protected, listings, prune)
	for _, repo := range prune {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune")
		proc.ConfigureNoWindow(command)
		_ = command.Run()
		cancel()
	}
	retention.PruneState(root, current, checkCeiling)
	return nil
}

func recoverRetiringWorktrees(root string) error {
	dir := filepath.Join(root, "worktree_ownership")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return readErr
		}
		var owned retention.WorktreeOwnership
		if json.Unmarshal(data, &owned) != nil || !owned.Retiring || len(owned.Jobs) == 0 {
			continue
		}
		if !validOwnedWorktree(root, owned.WorktreePath) {
			continue
		}
		if !owned.Claimed {
			_, err = retention.UpdateWorktreeOwnership(root, owned.WorktreePath, func(record *retention.WorktreeOwnership) { record.Claimed = true })
			if err != nil {
				continue
			}
		}
		id := owned.Jobs[0]
		job := readCleanupJSON(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"))
		if job == nil {
			continue
		}
		var retireErr error
		if generation, ok := cleanupNumber(job["generation"]); ok && generation > 0 {
			retireErr = RetireWorktree(root, id, int64(generation))
		} else {
			retireErr = RetireWorktree(root, id)
		}
		if retireErr != nil {
			continue
		}
	}
	return nil
}

func cleanupMayRemove(j map[string]any, state string) bool {
	return state != "UNKNOWN_OR_IN_PROGRESS" && cleanupTerminal[state] && (j["applyState"] == "applied" || j["discarded"] == true)
}

func cleanupEligible(j map[string]any, state string, now float64, path string) bool {
	if state == "UNKNOWN_OR_IN_PROGRESS" || !cleanupTerminal[state] || cleanupRecentJobConflict(j, state, now, path) {
		return false
	}
	if cleanupMayRemove(j, state) {
		return true
	}
	stamp, ok := cleanupNumber(j["finishedAt"])
	if !ok || stamp <= 0 {
		if info, err := os.Stat(path); err == nil {
			stamp = float64(info.ModTime().UnixNano()) / 1e9
		} else {
			return false
		}
	}
	return now-stamp >= cleanupRetention.Seconds()
}

func cleanupConflictState(job map[string]any, state string) bool {
	if state == "" {
		state, _ = job["state"].(string)
	}
	if state == "BLOCKED" || state == "RESOLVING" || state == "CONFLICT" {
		return true
	}
	conflict, _ := job["applyState"].(string)
	return conflict == "BLOCKED" || conflict == "RESOLVING" || conflict == "CONFLICT" || conflict == "conflict"
}

func cleanupRecentConflict(root string, job map[string]any, now float64) bool {
	id, _ := job["jobId"].(string)
	base := filepath.Join(root, "preparations", id)
	if !cleanupPath(root, base) || isCleanupLink(base) {
		return false
	}
	conflict := false
	_ = filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil || isCleanupLink(path) {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || filepath.Base(path) != "state.json" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		var stateRecord map[string]any
		if json.Unmarshal(data, &stateRecord) != nil || !cleanupConflictState(stateRecord, "") {
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr == nil && now-float64(info.ModTime().UnixNano())/1e9 < cleanupRetention.Seconds() {
			conflict = true
			return filepath.SkipAll
		}
		return nil
	})
	return conflict
}

func cleanupConflictProtected(root string, job map[string]any, state string, now float64, path string) bool {
	return cleanupRecentJobConflict(job, state, now, path) || cleanupRecentConflict(root, job, now)
}

func cleanupRecentJobConflict(job map[string]any, state string, now float64, path string) bool {
	if cleanupConflictState(job, state) {
		stamp, ok := cleanupNumber(job["finishedAt"])
		if !ok || stamp <= 0 {
			if info, err := os.Stat(path); err == nil {
				stamp = float64(info.ModTime().UnixNano()) / 1e9
			}
		}
		return stamp <= 0 || now-stamp < cleanupRetention.Seconds()
	}
	return false
}

func latestThreadJobs(records []cleanupRecord) []cleanupRecord {
	latest := make(map[string]cleanupRecord)
	stamp := make(map[string]float64)
	for _, record := range records {
		thread, _ := record.job["threadId"].(string)
		if thread == "" {
			continue
		}
		value, ok := cleanupNumber(record.job["finishedAt"])
		if !ok {
			if info, err := os.Stat(record.path); err == nil {
				value = float64(info.ModTime().UnixNano()) / 1e9
			}
		}
		if previous, exists := stamp[thread]; !exists || value > previous {
			latest[thread], stamp[thread] = record, value
		}
	}
	result := make([]cleanupRecord, 0, len(latest))
	for _, record := range latest {
		result = append(result, record)
	}
	return result
}
func readCleanupJSON(p string) map[string]any {
	if !safeCleanupPath(p) {
		return nil
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v map[string]any
	if d.Decode(&v) != nil {
		return nil
	}
	return v
}
func cleanupNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	case float64:
		return n, true
	}
	return 0, false
}
func cleanupPath(root, p string) bool {
	root, err := filepath.Abs(root)
	if err != nil || !safeCleanupPath(root) {
		return false
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return false
	}
	rel, e := filepath.Rel(root, p)
	if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if isCleanupLink(cur) {
			return false
		}
	}
	return true
}
func safeCleanupPath(p string) bool {
	a, e := filepath.Abs(p)
	if e != nil {
		return false
	}
	cur := string(filepath.Separator)
	vol := filepath.VolumeName(a)
	if vol != "" {
		cur = vol + string(filepath.Separator)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(a, vol), string(filepath.Separator))
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		if isCleanupLink(cur) {
			return false
		}
	}
	return true
}
func isCleanupLink(p string) bool {
	fi, e := os.Lstat(p)
	if e != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return isReparsePoint(fi)
}
func removeSafe(root, p string) bool {
	if !cleanupPath(root, p) {
		return false
	}
	fi, e := os.Lstat(p)
	if e != nil || !fi.Mode().IsRegular() {
		return false
	}
	return os.Remove(p) == nil
}
func regularFile(p string) bool { fi, e := os.Lstat(p); return e == nil && fi.Mode().IsRegular() }
func pathExists(p string) bool {
	if p == "" {
		return false
	}
	_, e := os.Lstat(p)
	return e == nil
}
func validProjectKey(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func worktreeKey(v any) string {
	p, ok := v.(string)
	if !ok || p == "" {
		return ""
	}
	a, e := filepath.Abs(p)
	if e != nil {
		return ""
	}
	r, e := filepath.EvalSymlinks(a)
	if e == nil {
		a = r
	}
	a = filepath.Clean(a)
	if runtime.GOOS == "windows" {
		a = strings.ToLower(a)
	}
	return a
}
func gitWorktreeListing(repo string) *worktreeListing {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "git", "-C", repo, "worktree", "list", "--porcelain")
	proc.ConfigureNoWindow(c)
	b, e := c.Output()
	if e != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			p, _ := filepath.Abs(strings.TrimPrefix(line, "worktree "))
			paths = append(paths, filepath.Clean(p))
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return &worktreeListing{paths[0], paths}
}
func removeWorktree(root, repo, wt string, listings map[string]*worktreeListing, prune map[string]string) bool {
	wtRoot := filepath.Join(root, "wt")
	target, e := filepath.Abs(wt)
	if e != nil || !cleanupPath(root, target) {
		return false
	}
	rr, e := filepath.Rel(wtRoot, target)
	if e != nil || rr == "." || rr == ".." || strings.HasPrefix(rr, ".."+string(filepath.Separator)) {
		return false
	}
	key := worktreeKey(repo)
	if key == "" {
		return false
	}
	if _, e = os.Stat(target); os.IsNotExist(e) {
		prune[key] = key
		return false
	}
	if listings[key] == nil {
		listings[key] = gitWorktreeListing(key)
	}
	l := listings[key]
	if l == nil {
		return false
	}
	found := false
	for _, p := range l.paths {
		if strings.EqualFold(filepath.Clean(p), filepath.Clean(target)) {
			found = true
		}
	}
	if !found {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "-C", l.command, "worktree", "remove", "--force", target)
	proc.ConfigureNoWindow(command)
	if command.Run() != nil {
		if !makeCleanupWritable(target) || command.Run() != nil {
			return false
		}
	}
	next := l.paths[:0]
	for _, p := range l.paths {
		if !strings.EqualFold(filepath.Clean(p), filepath.Clean(target)) {
			next = append(next, p)
		}
	}
	l.paths = next
	return true
}

func makeCleanupWritable(root string) bool {
	if isCleanupLink(root) {
		return false
	}
	ok := true
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || isCleanupLink(path) {
			ok = false
			return filepath.SkipDir
		}
		mode := os.FileMode(0600)
		if entry.IsDir() {
			mode = 0700
		}
		if os.Chmod(path, mode) != nil {
			ok = false
		}
		return nil
	})
	return ok
}

func enforceArtifactSizeLimit(root string, now float64, records []cleanupRecord, protected map[string]bool, listings map[string]*worktreeListing, prune map[string]string) {
	limit := maxArtifactBytes
	wtRoot, prepRoot := filepath.Join(root, "wt"), filepath.Join(root, "preparations")
	total := cleanupTreeBytes(wtRoot) + cleanupTreeBytes(prepRoot)
	if total <= limit {
		return
	}
	type candidate struct {
		record       cleanupRecord
		stamp        float64
		worktreeSize int64
		prepSize     int64
	}
	var candidates []candidate
	seenWT := map[string]bool{}
	inUseWT := map[string]bool{}
	for _, record := range records {
		job := record.job
		state, _ := job["state"].(string)
		inUse := state == "UNKNOWN_OR_IN_PROGRESS" || !cleanupTerminal[state] || cleanupConflictProtected(root, job, state, now, record.path)
		if !inUse && cleanupTerminal[state] && job["applyState"] != "applied" {
			stamp, ok := cleanupNumber(job["finishedAt"])
			if !ok || stamp <= 0 {
				if info, err := os.Stat(record.path); err == nil {
					stamp = float64(info.ModTime().UnixNano()) / 1e9
				}
			}
			inUse = stamp > 0 && now-stamp < (24*time.Hour).Seconds()
		}
		if inUse {
			if key := worktreeKey(job["worktreePath"]); key != "" {
				inUseWT[key] = true
			}
		}
	}
	for _, record := range records {
		job := record.job
		state, _ := job["state"].(string)
		if state == "UNKNOWN_OR_IN_PROGRESS" || !cleanupTerminal[state] {
			continue
		}
		stamp, ok := cleanupNumber(job["finishedAt"])
		if !ok || stamp <= 0 {
			if info, err := os.Stat(record.path); err == nil {
				stamp = float64(info.ModTime().UnixNano()) / 1e9
			}
		}
		if cleanupConflictProtected(root, job, state, now, record.path) {
			continue
		}
		if job["applyState"] != "applied" && stamp > 0 && now-stamp < (24*time.Hour).Seconds() {
			continue
		}
		wt, _ := job["worktreePath"].(string)
		key := worktreeKey(wt)
		if inUseWT[key] {
			continue
		}
		worktreeSize, prepSize := int64(0), int64(0)
		if wt != "" && key != "" && !seenWT[key] {
			worktreeSize = cleanupTreeBytes(wt)
			seenWT[key] = true
		}
		id, _ := job["jobId"].(string)
		prepSize = cleanupTreeBytes(filepath.Join(prepRoot, id))
		if worktreeSize+prepSize > 0 {
			candidates = append(candidates, candidate{record, stamp, worktreeSize, prepSize})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].stamp != candidates[j].stamp {
			return candidates[i].stamp < candidates[j].stamp
		}
		return candidates[i].record.path < candidates[j].record.path
	})
	for _, item := range candidates {
		if total <= limit {
			break
		}
		job := item.record.job
		wt, _ := job["worktreePath"].(string)
		removedWT := false
		if wt != "" {
			if repo, ok := job["repoRoot"].(string); ok && repo != "" {
				removedWT = removeWorktree(root, repo, wt, listings, prune)
			}
		}
		id, _ := job["jobId"].(string)
		prep := filepath.Join(prepRoot, id)
		removedPrep := cleanupPath(root, prep) && retention.RemovePreparationTree(root, prep)
		if removedWT {
			total -= item.worktreeSize
		}
		if removedPrep {
			total -= item.prepSize
		}
	}
	if total > limit {
		retention.Warning(root)
	}
}

func cleanupTreeBytes(root string) int64 {
	if root == "" || isCleanupLink(root) {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || isCleanupLink(path) {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func cleanupOrphanWorktrees(root string, current float64, protected map[string]bool, listings map[string]*worktreeListing, prune map[string]string, owners map[string]map[string]any) {
	base := filepath.Join(root, "wt")
	if isCleanupLink(base) {
		return
	}
	items, e := os.ReadDir(base)
	if e != nil {
		return
	}
	for _, it := range items {
		p := filepath.Join(base, it.Name())
		fi, e := os.Lstat(p)
		key := worktreeKey(p)
		if e != nil || !fi.IsDir() || isCleanupLink(p) || protected[key] || owners[key] != nil || current-float64(fi.ModTime().UnixNano())/1e9 <= cleanupRetention.Seconds() {
			continue
		}
		common := cleanupGitOutput(p, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if common == "" {
			continue
		}
		repo := filepath.Dir(filepath.Clean(common))
		removeWorktree(root, repo, p, listings, prune)
	}
}

func enforceWorktreeCount(root string, current float64, protected map[string]bool, listings map[string]*worktreeListing, prune map[string]string, owners map[string]map[string]any) {
	base := filepath.Join(root, "wt")
	if isCleanupLink(base) {
		return
	}
	items, e := os.ReadDir(base)
	if e != nil {
		return
	}
	type candidate struct {
		path  string
		stamp float64
		owner map[string]any
	}
	var all, candidates []candidate
	for _, it := range items {
		p := filepath.Join(base, it.Name())
		fi, e := os.Lstat(p)
		if e != nil || !fi.IsDir() || isCleanupLink(p) {
			continue
		}
		stamp := float64(fi.ModTime().UnixNano()) / 1e9
		c := candidate{p, stamp, nil}
		all = append(all, c)
		k := worktreeKey(p)
		if protected[k] {
			continue
		}
		owner := owners[k]
		if owner != nil {
			state, _ := owner["state"].(string)
			id, _ := owner["jobId"].(string)
			if !cleanupEligible(owner, state, current, filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")) {
				continue
			}
			idx := readCleanupJSON(filepath.Join(root, "async_jobs", id+".json"))
			if idx != nil && idx["stage"] == "FINALIZING" {
				continue
			}
		} else if current-stamp < cleanupRetention.Seconds() {
			continue
		}
		c.owner = owner
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].stamp != candidates[j].stamp {
			return candidates[i].stamp < candidates[j].stamp
		}
		return candidates[i].path < candidates[j].path
	})
	count := len(all)
	for _, c := range candidates {
		if count <= maxWorktrees {
			break
		}
		repo := ""
		if c.owner != nil {
			repo, _ = c.owner["repoRoot"].(string)
		} else {
			common := cleanupGitOutput(c.path, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if common != "" {
				repo = filepath.Dir(filepath.Clean(common))
			}
		}
		if repo != "" && removeWorktree(root, repo, c.path, listings, prune) {
			count--
		}
	}
	if count > maxWorktrees {
		retention.Warning(root)
	}
}

func cleanupGitOutput(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	proc.ConfigureNoWindow(cmd)
	b, e := cmd.Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
