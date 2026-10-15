package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
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

const cleanupRetention = 3 * 24 * time.Hour

var maxWorktrees = 128

var maxArtifactBytes int64 = 2 * 1024 * 1024 * 1024

var cleanupTerminal = map[string]bool{"SUCCEEDED": true, "FAILED": true, "CANCELLED": true, "TIMED_OUT": true, "UNKNOWN_OR_IN_PROGRESS": true, "EXPIRED": true, "SUPERSEDED": true}

var cleanupChildAlive = IdentityMatches
var cleanupMinimumInterval = 10 * time.Minute
var cleanupClock = time.Now
var cleanupSleep = time.Sleep

type cleanupChild struct {
	PID           int
	Identity      int64
	Completed     string
	LastCompleted int64
}

// RequestCleanup only stamps and launches. The stamp is never removed: a request
// racing acknowledgement of a pass remains different from Completed.
func RequestCleanup(root string, launch func() (int, int64, error)) (err error) {
	dir, err := cleanupDirectory(root)
	if err != nil {
		return err
	}
	defer func() { cleanupFailure(dir, err) }()
	if err = atomicWrite(filepath.Join(dir, "dirty"), []byte(rand.Text())); err != nil {
		return err
	}
	unlock, err := cleanupLock(dir, "queue")
	if err != nil {
		return err
	}
	defer unlock()
	child, err := readCleanupChild(dir)
	if err != nil {
		return err
	}
	if cleanupChildAlive(child.PID, child.Identity) {
		return nil
	}
	return launchCleanupChild(dir, child, launch)
}

// RunCleanup executes at most two existing bounded passes, then hands any newer
// stamp to a successor. Only the detached command calls this entry point.
func RunCleanup(root string, launch func() (int, int64, error)) error {
	return runCleanup(root, launch, func() error {
		if err := cleanupJobs(root, os.TempDir(), time.Now(), true); err != nil {
			return err
		}
		if executable, err := os.Executable(); err == nil {
			retention.PrunePluginCache(executable)
		}
		return nil
	})
}

func runCleanup(root string, launch func() (int, int64, error), pass func() error) (err error) {
	dir, err := cleanupDirectory(root)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("cleanup panic: %v", p)
		}
		cleanupFailure(dir, err)
	}()
	releaseSweep, err := cleanupLock(dir, "sweep")
	if err != nil {
		return err
	}
	defer func() {
		if releaseSweep != nil {
			releaseSweep()
		}
	}()
	for i := 0; i < 2; i++ {
		unlock, lockErr := cleanupLock(dir, "queue")
		if lockErr != nil {
			return lockErr
		}
		child, readErr := readCleanupChild(dir)
		dirty, stampErr := os.ReadFile(filepath.Join(dir, "dirty"))
		unlock()
		if readErr != nil {
			return readErr
		}
		if stampErr != nil {
			return stampErr
		}
		if string(dirty) != child.Completed {
			if child.LastCompleted > 0 {
				remaining := cleanupMinimumInterval - cleanupClock().Sub(time.Unix(child.LastCompleted, 0))
				if remaining > 0 {
					cleanupSleep(remaining)
				}
			}
			if err = pass(); err != nil {
				return err
			}
		}
		unlock, err = cleanupLock(dir, "queue")
		if err != nil {
			return err
		}
		child.Completed = string(dirty)
		child.LastCompleted = cleanupClock().Unix()
		latest, readErr := os.ReadFile(filepath.Join(dir, "dirty"))
		if readErr != nil {
			unlock()
			return readErr
		}
		if string(latest) == child.Completed || i == 1 {
			// Release the sweep under the queue lock so the exit/launch boundary
			// cannot strand a request behind an exiting child's PID.
			releaseSweep()
			releaseSweep = nil
			child.PID, child.Identity = 0, 0
			if string(latest) != child.Completed {
				err = launchCleanupChild(dir, child, launch)
			} else {
				err = writeCleanupChild(dir, child)
			}
			unlock()
			return err
		}
		err = writeCleanupChild(dir, child)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func cleanupDirectory(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "cleanup")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
		return "", fmt.Errorf("linked cleanup directory")
	}
	return dir, nil
}

func cleanupLock(dir, name string) (func(), error) {
	deadline := time.Now().Add(time.Second)
	for {
		f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err == nil {
			err = contextio.TryLock(f)
			if err == nil {
				return func() { _ = contextio.Unlock(f); _ = f.Close() }, nil
			}
			_ = f.Close()
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		// Sharing violations (including antivirus opens) are retries, not proof
		// that a running child will handle this request.
		time.Sleep(20 * time.Millisecond)
	}
}

// CleanupSweepLock exposes the same sweep lock used by RunCleanup.
func CleanupSweepLock(root string) (func(), error) {
	dir, err := cleanupDirectory(root)
	if err != nil {
		return nil, err
	}
	return cleanupLock(dir, "sweep")
}

// RetirementCandidate is a read-only decision from durable worktree ownership.
type RetirementCandidate struct {
	Path   string
	Jobs   []string
	Reason string
}

// ListRetirementCandidates inventories ownership using cleanup's retirement predicates.
func ListRetirementCandidates(root string, now time.Time) ([]RetirementCandidate, error) {
	dir := filepath.Join(root, "worktree_ownership")
	if !cleanupPath(root, dir) {
		return nil, fmt.Errorf("unsafe ownership directory")
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []RetirementCandidate
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		candidate := RetirementCandidate{Path: path, Reason: "invalid ownership"}
		var owned retention.WorktreeOwnership
		if cleanupPath(root, path) {
			data, readErr := os.ReadFile(path)
			if readErr == nil && json.Unmarshal(data, &owned) == nil {
				candidate.Path, candidate.Jobs = owned.WorktreePath, owned.Jobs
				if validOwnedWorktree(root, owned.WorktreePath) && len(owned.Jobs) > 0 {
					verified, e := retention.ReadWorktreeOwnership(root, owned.WorktreePath)
					verifiedJSON, _ := json.Marshal(verified)
					ownedJSON, _ := json.Marshal(owned)
					if e == nil && bytes.Equal(verifiedJSON, ownedJSON) {
						candidate.Reason = ""
						for _, id := range owned.Jobs {
							p := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
							if !validJobID(id) || len(id) != 64 || !cleanupPath(root, p) {
								candidate.Reason = "invalid job ownership"
								break
							}
							job := readCleanupJSON(p)
							state, _ := job["state"].(string)
							if job == nil || job["jobId"] != id || !cleanupEligible(job, state, float64(now.UnixNano())/1e9, p) || cleanupConflictProtected(root, job, state, float64(now.UnixNano())/1e9, p) {
								candidate.Reason = "owner is missing, protected or ineligible"
								break
							}
						}
						if !worktreeRetirableNow(root, owned.WorktreePath) {
							candidate.Reason = "referenced by ineligible job"
						}
					}
				}
			}
		}
		result = append(result, candidate)
	}
	return result, nil
}

func readCleanupChild(dir string) (cleanupChild, error) {
	var child cleanupChild
	data, err := os.ReadFile(filepath.Join(dir, "child.json"))
	if os.IsNotExist(err) {
		return child, nil
	}
	if err != nil {
		return child, err
	}
	err = json.Unmarshal(data, &child)
	return child, err
}

func writeCleanupChild(dir string, child cleanupChild) error {
	data, err := json.Marshal(child)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "child.json"), data)
}

func launchCleanupChild(dir string, child cleanupChild, launch func() (int, int64, error)) error {
	pid, identity, err := launch()
	if err != nil {
		return err
	}
	child.PID, child.Identity = pid, identity
	return writeCleanupChild(dir, child)
}

func cleanupFailure(dir string, err error) {
	if err != nil {
		_ = atomicWrite(filepath.Join(dir, "error"), []byte(err.Error()))
		fmt.Fprintf(os.Stderr, "Purser: cleanup retry pending: %v\n", err)
	}
}

type cleanupRecord struct {
	path string
	job  map[string]any
}
type worktreeListing struct {
	command string
	paths   []string
}

// RecoverCleanup performs prompt job and interrupted worktree recovery without
// sweeping caches or retiring other artifacts. Its count shares cleanup's budget.
func RecoverCleanup(stateRoot string, now time.Time) (count int, recoveryErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			recoveryErr = fmt.Errorf("recovery panic: %v", recovered)
		}
	}()
	recovered, err := RecoverPending(stateRoot, now)
	if err != nil {
		return recovered, fmt.Errorf("recover pending jobs: %w", err)
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return recovered, fmt.Errorf("resolve state root: %w", err)
	}
	if err := recoverRetiringWorktrees(root); err != nil {
		return recovered, fmt.Errorf("recover retiring worktrees: %w", err)
	}
	return recovered, nil
}

// CleanupJobs performs a locked cleanup pass without requiring Python.
func CleanupJobs(stateRoot string, now time.Time, checkCeiling bool) (cleanupErr error) {
	dir, err := cleanupDirectory(stateRoot)
	if err != nil {
		return err
	}
	unlock, err := cleanupLock(dir, "sweep")
	if err != nil {
		return err
	}
	defer unlock()
	return cleanupJobs(stateRoot, os.TempDir(), now, checkCeiling)
}

func cleanupJobs(stateRoot, tempRoot string, now time.Time, checkCeiling bool) (cleanupErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cleanupErr = fmt.Errorf("panic: %v", recovered)
		}
		if cleanupErr != nil {
			message := strings.NewReplacer("\r", " ", "\n", " ").Replace(cleanupErr.Error())
			fmt.Fprintf(os.Stderr, "Purser: job cleanup aborted: %s\n", message)
		}
	}()
	recovered, err := RecoverCleanup(stateRoot, now)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return fmt.Errorf("resolve state root: %w", err)
	}
	sweepTemporaryDirectories(root, now)
	retention.RemoveOldPrefixedEntries(filepath.Join(root, "cache", "go-tmp"), now, 24*time.Hour, []string{""})
	retention.RemoveOldPrefixedEntries(tempRoot, now, 24*time.Hour, []string{"purser-", "reviewer1-", "readiness-check-", "go-build"})
	if err := trimPurserGoCache(root); err != nil {
		fmt.Fprintf(os.Stderr, "Purser: cache cleanup: %v\n", err)
	}
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read jobs directory: %w", err)
	}
	current := float64(now.UnixNano()) / 1e9
	loaded := make([]cleanupRecord, 0)
	worktreeJobs := map[string][]cleanupRecord{}
	protected := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(jobsDir, e.Name())
		j := readCleanupJSON(p)
		stem := strings.TrimSuffix(e.Name(), ".json")
		if j == nil || j["jobId"] != stem {
			// An unreadable owner may reference any worktree; leave it for recovery.
			retention.Warning(root)
			return nil
		}
		if j["jobId"] == stem {
			record := cleanupRecord{p, j}
			loaded = append(loaded, record)
			if k := worktreeKey(j["worktreePath"]); k != "" {
				worktreeJobs[k] = append(worktreeJobs[k], record)
			}
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
				acted = removeWorktree(root, repo, wt, listings, prune, worktreeJobs) || acted
			}
		}
		stem := strings.TrimSuffix(filepath.Base(r.path), ".json")
		if state != "UNKNOWN_OR_IN_PROGRESS" {
			p := filepath.Join(jobsDir, stem+".launch")
			if removeSafe(root, p) {
				acted = true
			}
		}
		if wt == "" {
			// Retirement clears bindings before removal; ownership remains retry authority.
			var ownedErr error
			wt, ownedErr = findOwnedWorktree(root, stem)
			if ownedErr != nil && ownedErr != errRetirementOwnershipMissing {
				continue
			}
		}
		_, worktreeErr := os.Lstat(wt)
		if ((j["worktreePath"] != nil || wt != "") && (wt == "" || !cleanupPath(root, wt) || !os.IsNotExist(worktreeErr))) || !r.expired || !distilled[learning.SourceKey(stem)] {
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
	cleanupOrphanWorktrees(root, current, protected, listings, prune, owners, worktreeJobs)
	enforceWorktreeCount(root, current, protected, listings, prune, owners, worktreeJobs)
	byteCheck := false
	if checkCeiling {
		if fi, e := os.Stat(filepath.Join(root, retention.CeilingStamp)); e != nil {
			byteCheck = true
		} else {
			byteCheck = current-float64(fi.ModTime().UnixNano())/1e9 >= 86400
		}
	}
	retention.RetirePreparations(root, current, byteCheck)
	enforceArtifactSizeLimit(root, current, loaded, protected, listings, prune, worktreeJobs)
	for _, repo := range prune {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := proc.Hide(exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune"))
		proc.ConfigureNoWindow(command)
		_ = command.Run()
		cancel()
	}
	retention.PruneState(root, current, checkCeiling)
	return nil
}

func sweepTemporaryDirectories(root string, now time.Time) {
	base := filepath.Join(root, "cache", "tmp")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	for _, entry := range entries {
		path := filepath.Join(base, entry.Name())
		info, err := entry.Info()
		if err != nil || !info.IsDir() || now.Sub(info.ModTime()) < time.Hour {
			continue
		}
		job := readCleanupJSON(filepath.Join(jobsDir, entry.Name()+".json"))
		if job != nil {
			state, _ := job["state"].(string)
			if !cleanupTerminal[state] {
				continue
			}
		}
		_ = os.RemoveAll(path)
	}
}

func trimPurserGoCache(root string) error {
	unlock, err := cacheGate(root)
	if err != nil {
		return err
	}
	defer unlock()
	users := filepath.Join(root, "cache", "go-users")
	entries, err := os.ReadDir(users)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid cache user: %s", entry.Name())
		}
		f, e := os.OpenFile(filepath.Join(users, entry.Name()), os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		e = contextio.TryLock(f)
		if e == nil {
			_ = contextio.Unlock(f)
		}
		_ = f.Close()
		if e != nil {
			return nil
		}
	}
	if hasRunningCodexJob(root) {
		return nil
	}
	cache := filepath.Join(root, "cache", "go-build")
	if _, statErr := os.Lstat(cache); os.IsNotExist(statErr) {
		return nil
	} else if statErr != nil || !cleanupPath(root, cache) {
		return fmt.Errorf("invalid Purser cache: %s", cache)
	}
	var size int64
	err = filepath.WalkDir(cache, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if isCleanupLink(path) {
			return fmt.Errorf("cache link: %s", path)
		}
		if !entry.IsDir() {
			if info, e := entry.Info(); e == nil && info.Mode().IsRegular() {
				size += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if size <= 2*1024*1024*1024 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := proc.Hide(exec.CommandContext(ctx, "go", "clean", "-cache"))
	command.Env = os.Environ()
	found := false
	for index, entry := range command.Env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "GOCACHE") {
			command.Env[index] = "GOCACHE=" + cache
			found = true
		}
	}
	if !found {
		command.Env = append(command.Env, "GOCACHE="+cache)
	}
	proc.ConfigureNoWindow(command)
	var stderr cleanupStderr
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("go clean -cache: %w: %s", err, stderr.String())
	}
	return nil
}

func hasRunningCodexJob(root string) bool {
	entries, err := os.ReadDir(filepath.Join(root, "codex_exec_mcp", "jobs"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(root, "codex_exec_mcp", "jobs", entry.Name())
		job := readCleanupJSON(path)
		if job != nil && (job["state"] == "RUNNING" || job["state"] == "UNKNOWN_OR_IN_PROGRESS") {
			return true
		}
		if job == nil {
			info, e := entry.Info()
			if e == nil && time.Since(info.ModTime()) < time.Hour {
				fmt.Fprintf(os.Stderr, "Purser: malformed recent job record blocks cache cleanup: %s\n", path)
				return true
			}
			fmt.Fprintf(os.Stderr, "Purser: ignoring stale malformed job record for cache cleanup: %s\n", path)
		}
	}
	return false
}

func cacheGate(root string) (func() error, error) {
	path := filepath.Join(root, "cache", "go-cache.lock")
	if !cleanupPath(root, path) {
		return nil, fmt.Errorf("invalid cache gate: %s", path)
	}
	return contextio.LockFileBlocking(path)
}

func acquireCacheUser(root string) (func(), error) {
	gate, err := cacheGate(root)
	if err != nil {
		return nil, err
	}
	defer gate()
	dir := filepath.Join(root, "cache", "go-users")
	if !cleanupPath(root, dir) {
		return nil, fmt.Errorf("invalid cache users: %s", dir)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "user-")
	if err != nil {
		return nil, err
	}
	if err := contextio.TryLock(f); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return func() { _ = contextio.Unlock(f); _ = f.Close(); _ = os.Remove(f.Name()) }, nil
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
	return state != "UNKNOWN_OR_IN_PROGRESS" && cleanupTerminal[state] && (state == "CANCELLED" || j["applyState"] == "applied" || j["applyState"] == "rejected" || j["applyState"] == "superseded" || j["discarded"] == true)
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
	if err != nil {
		return false
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || !safeCleanupPath(root) {
		return false
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || isReparsePoint(rootInfo) {
		return false
	}
	resolvedPath, err := resolveCleanupPath(p)
	if err != nil {
		return false
	}
	lexicalRel, e := filepath.Rel(root, p)
	if e != nil || lexicalRel == "." || lexicalRel == ".." || strings.HasPrefix(lexicalRel, ".."+string(filepath.Separator)) {
		return false
	}
	resolvedRel, e := filepath.Rel(resolvedRoot, resolvedPath)
	if e != nil || resolvedRel == "." || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
		return false
	}
	cur := root
	for _, part := range strings.Split(lexicalRel, string(filepath.Separator)) {
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
	if isCleanupLink(a) {
		return false
	}
	_, e = resolveCleanupPath(a)
	return e == nil
}
func resolveCleanupPath(p string) (string, error) {
	a, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		return filepath.Clean(resolved), nil
	}
	suffix := []string{}
	for {
		parent := filepath.Dir(a)
		if parent == a {
			return "", os.ErrNotExist
		}
		suffix = append(suffix, filepath.Base(a))
		a = parent
		if resolved, err := filepath.EvalSymlinks(a); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
	}
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
	c := proc.Hide(exec.CommandContext(ctx, "git", "-C", repo, "worktree", "list", "--porcelain"))
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
func removeWorktree(root, repo, wt string, listings map[string]*worktreeListing, prune map[string]string, passJobs ...map[string][]cleanupRecord) bool {
	target, e := filepath.Abs(wt)
	if e != nil || !cleanupPath(root, target) {
		return false
	}
	wtRootKey, targetKey := worktreeKey(filepath.Join(root, "wt")), worktreeKey(target)
	if wtRootKey == "" || targetKey == "" {
		return false
	}
	rr, e := filepath.Rel(wtRootKey, targetKey)
	if e != nil || rr == "." || rr == ".." || strings.HasPrefix(rr, ".."+string(filepath.Separator)) {
		return false
	}
	key := worktreeKey(repo)
	if _, e = os.Stat(target); os.IsNotExist(e) {
		if key != "" {
			prune[key] = key
		}
		return true
	}
	_, gitErr := os.Lstat(filepath.Join(target, ".git"))
	missingGit := os.IsNotExist(gitErr)
	if !missingGit && key == "" {
		return false
	}
	if !missingGit {
		info, repoErr := os.Stat(key)
		if repoErr != nil || !info.IsDir() {
			return false
		}
	}
	jobRecords := cleanupRecordsForWorktree(root, target, passJobs...)
	if !worktreeRemovalReady(jobRecords) {
		return false
	}
	if !missingGit && listings[key] == nil {
		listings[key] = gitWorktreeListing(key)
	}
	l := listings[key]
	owned, ownerErr := retention.ReadWorktreeOwnership(root, target)
	ownedOK := ownerErr == nil && sameWorktree(owned.WorktreePath, target) && len(owned.Jobs) > 0
	fallbackOK := ownedOK && l != nil
	if !missingGit && l == nil && !fallbackOK {
		recordWorktreeRemoval(jobRecords, target, fmt.Errorf("git worktree listing unavailable"))
		return false
	}
	found := false
	if l != nil {
		for _, p := range l.paths {
			if worktreeKey(p) == worktreeKey(target) {
				found = true
			}
		}
	}
	if !missingGit && !found && !fallbackOK {
		recordWorktreeRemoval(jobRecords, target, fmt.Errorf("worktree is not registered"))
		return false
	}

	var removalErr error
	for attempt := 0; attempt < 2; attempt++ {
		var stderr cleanupStderr
		if found && !missingGit {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			command := proc.Hide(exec.CommandContext(ctx, "git", "-C", l.command, "worktree", "remove", "--force", target))
			proc.ConfigureNoWindow(command)
			command.Stderr = &stderr
			removalErr = command.Run()
			cancel()
			if removalErr != nil {
				removalErr = fmt.Errorf("%w: %s", removalErr, stderr.String())
			}
		} else {
			removalErr = os.RemoveAll(target)
		}
		if removalErr == nil {
			if _, statErr := os.Lstat(target); os.IsNotExist(statErr) {
				break
			}
			removalErr = fmt.Errorf("target remains after removal")
		}
		if attempt == 0 && !makeCleanupWritable(target) {
			break
		}
		if attempt == 1 || removalErr == nil {
			break
		}
	}
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		if removalErr == nil {
			removalErr = fmt.Errorf("target remains after removal")
		}
		recordWorktreeRemoval(jobRecords, target, removalErr)
		return false
	}
	if l != nil {
		next := l.paths[:0]
		for _, p := range l.paths {
			if worktreeKey(p) != worktreeKey(target) {
				next = append(next, p)
			}
		}
		l.paths = next
	}
	if missingGit && key != "" {
		prune[key] = key
	}
	return true
}

type cleanupStderr struct{ bytes.Buffer }

func (b *cleanupStderr) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		remaining := 4096 - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func recordWorktreeRemoval(records []cleanupRecord, target string, removalErr error) {
	message := fmt.Sprintf("remove %s: %v", target, removalErr)
	if len(message) > 4096 {
		message = message[:4096]
	}
	for _, record := range records {
		path, job := record.path, record.job
		bound, _ := job["worktreePath"].(string)
		if job == nil || (bound != "" && filepath.Clean(bound) != filepath.Clean(target) && !sameWorktree(bound, target)) {
			continue
		}
		attempts := 0.0
		if prior, ok := job["cleanupRemoval"].(map[string]any); ok {
			attempts, _ = cleanupNumber(prior["attempts"])
		}
		attempts++
		delay := 5 * time.Minute
		for n := 1.0; n < attempts && delay < time.Hour; n++ {
			delay *= 2
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		job["cleanupRemoval"] = map[string]any{"path": target, "error": message, "attempts": attempts, "nextAttemptAt": time.Now().Add(delay).Unix()}
		data, marshalErr := json.Marshal(job)
		if marshalErr == nil {
			_ = atomicWrite(path, data)
		}
	}
}

func worktreeRemovalReady(records []cleanupRecord) bool {
	now := float64(time.Now().Unix())
	for _, record := range records {
		job := record.job
		if retry, ok := job["cleanupRemoval"].(map[string]any); ok {
			if next, ok := cleanupNumber(retry["nextAttemptAt"]); ok && next > now {
				return false
			}
		}
	}
	return true
}

func cleanupRecordsForWorktree(root, target string, passJobs ...map[string][]cleanupRecord) []cleanupRecord {
	if len(passJobs) > 0 {
		return passJobs[0][worktreeKey(target)]
	}
	owned, err := retention.ReadWorktreeOwnership(root, target)
	if err != nil {
		return nil
	}
	records := make([]cleanupRecord, 0, len(owned.Jobs))
	for _, id := range owned.Jobs {
		path := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
		if job := readCleanupJSON(path); job != nil {
			records = append(records, cleanupRecord{path, job})
		}
	}
	return records
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

func enforceArtifactSizeLimit(root string, now float64, records []cleanupRecord, protected map[string]bool, listings map[string]*worktreeListing, prune map[string]string, worktreeJobs map[string][]cleanupRecord) {
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
				removedWT = removeWorktree(root, repo, wt, listings, prune, worktreeJobs)
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

func cleanupOrphanWorktrees(root string, current float64, protected map[string]bool, listings map[string]*worktreeListing, prune map[string]string, owners map[string]map[string]any, worktreeJobs map[string][]cleanupRecord) {
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
		common := ""
		if pathExists(filepath.Join(p, ".git")) {
			common = cleanupGitOutput(p, "rev-parse", "--path-format=absolute", "--git-common-dir")
		}
		repo := ""
		if common != "" {
			repo = filepath.Dir(filepath.Clean(common))
		}
		removeWorktree(root, repo, p, listings, prune, worktreeJobs)
	}
}

func enforceWorktreeCount(root string, current float64, protected map[string]bool, listings map[string]*worktreeListing, prune map[string]string, owners map[string]map[string]any, worktreeJobs map[string][]cleanupRecord) {
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
		} else if pathExists(filepath.Join(c.path, ".git")) {
			common := cleanupGitOutput(c.path, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if common != "" {
				repo = filepath.Dir(filepath.Clean(common))
			}
		}
		if removeWorktree(root, repo, c.path, listings, prune, worktreeJobs) {
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
	cmd := proc.Hide(exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...))
	proc.ConfigureNoWindow(cmd)
	b, e := cmd.Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
