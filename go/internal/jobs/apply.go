package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

// Apply applies a succeeded worktree job to its repository. stateRoot is the
// Purser state directory that contains codex_exec_mcp/jobs.
func Apply(stateRoot string, args map[string]any, options ...ApplyOptions) map[string]any {
	var opt ApplyOptions
	if len(options) != 0 {
		opt = options[0]
	}
	result := applyToolResult
	resolved := false
	if value, ok := args["resolved"]; ok {
		var valid bool
		resolved, valid = value.(bool)
		if !valid {
			return result(map[string]any{"applied": false, "reason": "resolved must be a boolean."})
		}
	}
	id, _ := args["jobId"].(string)
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	job := readJob(dir, id)
	if job == nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
	}
	var jobID, state, worktree, repo, snapshot string
	job.Get("jobId", &jobID)
	job.Get("state", &state)
	job.Get("worktreePath", &worktree)
	job.Get("repoRoot", &repo)
	job.Get("snapshot", &snapshot)
	var conflictReason string
	job.Get("applyConflictReason", &conflictReason)
	var discarded bool
	job.Get("discarded", &discarded)
	if discarded {
		return result(map[string]any{"jobId": jobID, "errorCode": "DISCARDED", "message": "This job was discarded and cannot be applied."})
	}
	if worktree == "" {
		return result(map[string]any{"state": "FAILED", "errorCode": "NOT_WORKTREE_JOB", "message": "Job has no worktree"})
	}
	if state != "SUCCEEDED" && !(resolved && state == "FAILED" && conflictReason == "worker_replay") {
		return result(map[string]any{"state": "FAILED", "errorCode": "JOB_NOT_READY", "message": "Job has not succeeded"})
	}
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "REPOSITORY_UNAVAILABLE", "message": err.Error()})
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "REPOSITORY_UNAVAILABLE", "message": err.Error()})
	}
	digest := sha256.Sum256([]byte(root))
	unlock, err := lease.Lock(filepath.Join(filepath.Dir(dir), "apply", hex.EncodeToString(digest[:])+".lock"))
	if err != nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "APPLY_LOCK_UNAVAILABLE", "message": err.Error()})
	}
	defer func() { _ = unlock() }()
	job = readJob(dir, jobID)
	if job == nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "JOB_NOT_FOUND", "message": "Job record is unavailable"})
	}
	var applyState string
	job.Get("applyState", &applyState)
	job.Get("applyConflictReason", &conflictReason)
	job.Get("worktreePath", &worktree)
	job.Get("repoRoot", &repo)
	job.Get("snapshot", &snapshot)
	var saved map[string]any
	if applyState == "applied" && job.Get("applyResult", &saved) && saved != nil {
		recordApplyDecision(opt, stateRoot, recordMap(job), "accepted", "apply", jobID, args["reviewer1Valid"], args["reviewer1False"])
		releaseAppliedJobLeases(opt, stateRoot, dir, worktree)
		return result(saved)
	}
	if resolved {
		if applyState != "conflict" && conflictReason != "worker_replay" {
			return result(map[string]any{"applied": false, "reason": "Job is not in conflict state."})
		}
		return applyResolvedJob(stateRoot, dir, jobID, job, repo, worktree, snapshot, args, opt)
	}
	if info, e := os.Stat(worktree); e != nil || !info.IsDir() {
		return result(map[string]any{"state": "FAILED", "errorCode": "WORKTREE_MISSING", "message": "Job worktree is unavailable"})
	}
	patch, err := ledger.JobPatch(worktree, snapshot)
	if err != nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "PATCH_UNAVAILABLE", "message": err.Error()})
	}
	paths, err := patchPaths(root, patch)
	if err != nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "PATCH_PATHS_UNAVAILABLE", "message": err.Error()})
	}
	if len(paths) != 0 {
		allPresent := true
		for _, path := range paths {
			if !sameWorktreeContent(worktree, root, path) {
				allPresent = false
				break
			}
		}
		if allPresent {
			return persistApplied(stateRoot, dir, jobID, job, map[string]any{"applied": true, "paths": paths}, args, opt)
		}
	}
	if _, ok := job.values["applyResult"]; ok {
		_ = job.Get("applyResult", &saved)
		return result(saved)
	}
	var reviewed string
	if job.Get("reviewedSnapshot", &reviewed) && reviewed != "" {
		drift, e := ledger.SnapshotDriftPaths(root, reviewed, paths)
		if e != nil {
			return result(map[string]any{"state": "FAILED", "errorCode": "SNAPSHOT_UNAVAILABLE", "message": e.Error()})
		}
		if len(drift) != 0 {
			var revision int64
			job.Get("snapshotRevision", &revision)
			_ = job.Set("snapshotRevision", revision+1)
			_ = job.Set("reviewRefreshPending", true)
			delete(job.values, "reviewedSnapshot")
			for i, key := range job.keys {
				if key == "reviewedSnapshot" {
					job.keys = append(job.keys[:i], job.keys[i+1:]...)
					break
				}
			}
			if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
				return result(map[string]any{"state": "FAILED", "errorCode": "JOB_WRITE_FAILED", "message": err.Error()})
			}
			index := filepath.Join(stateRoot, "async_jobs", jobID+".json")
			var value map[string]any
			b, e := os.ReadFile(index)
			if e == nil {
				e = json.Unmarshal(b, &value)
			}
			if e == nil && value == nil {
				e = errors.New("async job record must be a JSON object")
			}
			if e == nil {
				value["stage"] = "REVIEW_REFRESH"
				b, e = json.Marshal(value)
			}
			if e == nil {
				e = atomicWrite(index, b)
			}
			if e != nil {
				return result(map[string]any{"applied": false, "errorCode": "REVIEW_REFRESH_UNAVAILABLE", "message": "Job was re-prepared but review refresh could not be scheduled."})
			}
			return result(map[string]any{"applied": false, "errorCode": "REVIEW_STALE_REPREPARE_REQUIRED", "message": "Checkout changed in the reviewed job paths. Call codex_result to re-prepare and review the diff before applying.", "paths": drift})
		}
	}
	outcome := ledger.ApplyJobPatch(root, patch)
	applied, _ := outcome["applied"].(bool)
	value := map[string]any{"applied": applied}
	if applied {
		value["paths"] = paths
	} else {
		value["conflicts"] = outcome["conflicts"]
		value["applicable"] = outcome["applicable"]
		value["worktreePath"] = worktree
	}
	_ = job.Set("applyState", map[bool]string{true: "applied", false: "conflict"}[applied])
	_ = job.Set("applyResult", value)
	if applied {
		_ = job.Set("appliedAt", float64(time.Now().UnixNano())/1e9)
		if task, _ := recordMap(job)["taskId"].(string); task != "" {
			_ = job.Set("decisionRecorded", true)
		}
	}
	if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
		return result(map[string]any{"state": "FAILED", "errorCode": "JOB_WRITE_FAILED", "message": err.Error()})
	}
	if applied {
		recordScopeOutcome(opt, stateRoot, recordMap(job), "applied")
		recordApplyDecision(opt, stateRoot, recordMap(job), "accepted", "apply", jobID, args["reviewer1Valid"], args["reviewer1False"])
		releaseAppliedJobLeases(opt, stateRoot, dir, worktree)
	}
	return result(value)
}

func persistApplied(stateRoot, dir, jobID string, job *Record, value map[string]any, args map[string]any, opt ApplyOptions) map[string]any {
	_ = job.Set("applyState", "applied")
	_ = job.Set("applyResult", value)
	_ = job.Set("appliedAt", float64(time.Now().UnixNano())/1e9)
	if task, _ := recordMap(job)["taskId"].(string); task != "" {
		_ = job.Set("decisionRecorded", true)
	}
	if err := Write(filepath.Join(dir, jobID+".json"), job); err != nil {
		return applyToolResult(map[string]any{"state": "FAILED", "errorCode": "JOB_WRITE_FAILED", "message": err.Error()})
	}
	var worktree string
	job.Get("worktreePath", &worktree)
	recordScopeOutcome(opt, stateRoot, recordMap(job), "applied")
	recordApplyDecision(opt, stateRoot, recordMap(job), "accepted", "apply", jobID, args["reviewer1Valid"], args["reviewer1False"])
	releaseAppliedJobLeases(opt, stateRoot, dir, worktree)
	return applyToolResult(value)
}

func applyToolResult(value map[string]any) map[string]any {
	var text string
	b, err := json.Marshal(value)
	if err == nil {
		text = string(b)
	}
	_, isError := value["errorCode"]
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "structuredContent": value, "isError": isError}
}

func patchPaths(root, patch string) ([]string, error) { return ledger.JobPatchPaths(root, patch) }

func sameWorktreeContent(worktree, repo, path string) bool {
	a, b := filepath.Join(worktree, path), filepath.Join(repo, path)
	ai, ae := os.Lstat(a)
	bi, be := os.Lstat(b)
	if ae != nil || be != nil {
		return errors.Is(ae, os.ErrNotExist) && errors.Is(be, os.ErrNotExist)
	}
	if ai.Mode()&os.ModeSymlink != 0 || bi.Mode()&os.ModeSymlink != 0 {
		if ai.Mode()&os.ModeSymlink == 0 || bi.Mode()&os.ModeSymlink == 0 {
			return false
		}
		al, ae := os.Readlink(a)
		bl, be := os.Readlink(b)
		return ae == nil && be == nil && al == bl
	}
	if !ai.Mode().IsRegular() || !bi.Mode().IsRegular() {
		return false
	}
	x, ae := os.ReadFile(a)
	y, be := os.ReadFile(b)
	return ae == nil && be == nil && string(x) == string(y)
}
