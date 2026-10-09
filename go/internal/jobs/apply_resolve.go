package jobs

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
	"purser-hook/internal/proc"
)

// ApplyOptions injects the hand-resolution side effects for parity tests.
type ApplyOptions struct {
	RecordDecision             func(job map[string]any, decision, kind, sourceJobID string)
	RecordScopeConflictOutcome func(job map[string]any, action string)
	ReleaseScopeLease          func(stateRoot, jobID string) bool
}

func applyResolvedJob(stateRoot, dir, jobID string, job *Record, repo, worktree, snapshot string, args map[string]any, opt ApplyOptions) map[string]any {
	fail := func(reason string) map[string]any {
		return applyToolResult(map[string]any{"applied": false, "reason": reason})
	}
	patch, err := ledger.JobPatch(worktree, snapshot)
	if err != nil {
		return applyToolResult(map[string]any{"state": "FAILED", "errorCode": "PATCH_UNAVAILABLE", "message": err.Error()})
	}
	paths, err := ledger.JobPatchPaths(repo, patch)
	if err != nil {
		return applyToolResult(map[string]any{"state": "FAILED", "errorCode": "PATCH_PATHS_UNAVAILABLE", "message": err.Error()})
	}
	if len(paths) == 0 {
		return fail("Job has no changed paths.")
	}
	command := exec.Command("git", "diff", "--name-only", "--diff-filter=U", "-z")
	command.Dir = repo
	proc.ConfigureNoWindow(command)
	unmerged, err := command.Output()
	if err != nil {
		return fail("Repository has unmerged paths.")
	}
	if len(unmerged) != 0 {
		return fail("Repository has unmerged paths.")
	}
	for _, path := range paths {
		target, safe := resolvedJobPath(repo, path)
		if !safe {
			return fail("Job contains an unsafe path.")
		}
		info, err := os.Lstat(target)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		content, err := os.ReadFile(target)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(line, "<<<<<<<") || strings.HasPrefix(line, ">>>>>>>") {
				return fail("Conflict markers remain in " + path + ".")
			}
		}
	}
	value := map[string]any{"applied": true, "resolvedByHand": true, "paths": paths}
	_ = job.Set("applyState", "applied")
	_ = job.Set("applyResult", value)
	_ = job.Set("appliedAt", float64(time.Now().UnixNano())/1e9)
	values := recordMap(job)
	if task, ok := values["taskId"].(string); ok && task != "" {
		_ = job.Set("decisionRecorded", true)
	}
	if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
		return applyToolResult(map[string]any{"state": "FAILED", "errorCode": "JOB_WRITE_FAILED", "message": err.Error()})
	}
	values = recordMap(job)
	recordScopeOutcome(opt, stateRoot, values, "resolved_by_hand")
	recordApplyDecision(opt, stateRoot, values, "accepted", "apply", jobID, args["reviewer1Valid"], args["reviewer1False"])
	releaseAppliedJobLeases(opt, stateRoot, dir, worktree)
	dispatchWorktreeRetirement(stateRoot, jobID)
	return applyToolResult(value)
}

func resolvedJobPath(repo, name string) (string, bool) {
	if filepath.IsAbs(name) {
		return "", false
	}
	root, err := filepath.Abs(repo)
	if err != nil {
		return "", false
	}
	target := filepath.Join(root, filepath.FromSlash(name))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		ancestor, suffix := filepath.Dir(target), []string{filepath.Base(target)}
		for {
			parent, parentErr := filepath.EvalSymlinks(ancestor)
			if parentErr == nil {
				for i := len(suffix) - 1; i >= 0; i-- {
					parent = filepath.Join(parent, suffix[i])
				}
				resolved = parent
				break
			}
			next := filepath.Dir(ancestor)
			if next == ancestor {
				return "", false
			}
			suffix = append(suffix, filepath.Base(ancestor))
			ancestor = next
		}
	}
	rel, err = filepath.Rel(root, resolved)
	return resolved, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func recordScopeOutcome(opt ApplyOptions, root string, job map[string]any, action string) {
	defer func() { _ = recover() }()
	if opt.RecordScopeConflictOutcome != nil {
		opt.RecordScopeConflictOutcome(job, action)
	} else {
		enforce.RecordScopeConflictOutcome(root, job, action)
	}
}

func recordApplyDecision(opt ApplyOptions, root string, job map[string]any, decision, kind, source string, reviewerCounts ...any) {
	if task, _ := job["taskId"].(string); task == "" {
		return
	}
	if decision == "accepted" && kind == "apply" {
		if repository, _ := job["repoRoot"].(string); repository != "" {
			jobID, _ := job["jobId"].(string)
			_ = contextio.RecordPlanApply(root, repository, nonemptyString(job["taskId"]), jobID)
		}
	}
	defer func() { _ = recover() }()
	if opt.RecordDecision != nil {
		opt.RecordDecision(job, decision, kind, source)
	} else {
		enforce.RecordClaudeDecision(root, job, decision, kind, source, reviewerCounts...)
	}
}

func releaseAppliedJobLeases(opt ApplyOptions, stateRoot, dir, worktree string) {
	defer func() { _ = recover() }()
	release := opt.ReleaseScopeLease
	if release == nil {
		release = lease.ReleaseJobScopeLease
	}
	target, err := filepath.Abs(worktree)
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	target = filepath.Clean(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		bound, err := Read(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		values := recordMap(bound)
		boundPath, _ := values["worktreePath"].(string)
		if boundPath == "" {
			continue
		}
		boundTarget, err := filepath.Abs(boundPath)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(boundTarget); err == nil {
			boundTarget = resolved
		}
		boundTarget = filepath.Clean(boundTarget)
		id, _ := values["jobId"].(string)
		if id == "" || boundTarget != target {
			continue
		}
		if release(stateRoot, id) {
			var index map[string]any
			if raw, e := os.ReadFile(filepath.Join(stateRoot, "async_jobs", id+".json")); e == nil && json.Unmarshal(raw, &index) == nil {
				if call, ok := index["call_key"].(string); ok && call != "" {
					_ = os.Remove(filepath.Join(stateRoot, "pending", call+".json"))
				}
			}
		}
	}
}
