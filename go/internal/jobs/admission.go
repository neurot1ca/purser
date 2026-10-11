package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/enforce"
	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func init() {
	enforce.ReleaseHandAppliedBlockers = ReleaseHandAppliedBlockers
	enforce.RefreshHandAppliedJob = markHandAppliedJob
}

// ReleaseHandAppliedBlockers marks succeeded jobs applied when their worktree
// content is already present in the repository, then releases their lease.
func ReleaseHandAppliedBlockers(stateRoot, projectRoot string, scope []string) {
	if len(scope) == 0 {
		return
	}
	owners := handAppliedOwners(projectRoot, scope)
	for _, owner := range owners {
		markHandAppliedJob(stateRoot, owner.JobID)
	}
}

type handAppliedOwner struct {
	State string `json:"state"`
	Scope []any  `json:"scope"`
	JobID string `json:"job_id"`
}

func handAppliedOwners(projectRoot string, scope []string) []handAppliedOwner {
	unlock, err := lease.Lock(filepath.Join(projectRoot, "scope_leases.lock"))
	if err != nil {
		return nil
	}
	defer unlock()
	raw, err := os.ReadFile(filepath.Join(projectRoot, "scope_leases.json"))
	if err != nil || lease.Validate(raw) != nil {
		return nil
	}
	var state struct {
		Owners map[string]handAppliedOwner `json:"owners"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return nil
	}
	requested := make([]any, len(scope))
	for i, path := range scope {
		requested[i] = path
	}
	owners := make([]handAppliedOwner, 0, len(state.Owners))
	for _, owner := range state.Owners {
		if owner.JobID != "" && lease.ScopesOverlap(owner.Scope, requested) {
			owners = append(owners, owner)
		}
	}
	return owners
}

func markHandAppliedJob(stateRoot, jobID string) {
	if len(jobID) != sha256.Size*2 {
		return
	}
	if _, err := hex.DecodeString(jobID); err != nil {
		return
	}
	dir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	path := filepath.Join(dir, jobID+".json")
	prior := readJob(dir, jobID)
	if prior == nil {
		return
	}
	var priorState, priorApply, priorError string
	prior.Get("state", &priorState)
	prior.Get("applyState", &priorApply)
	prior.Get("errorCode", &priorError)
	if priorApply == "applied" || (priorState != "SUCCEEDED" && !(priorState == "FAILED" && priorError == "PATCH_UNAVAILABLE")) {
		return
	}
	var repo string
	prior.Get("repoRoot", &repo)
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return
	}
	digest := sha256.Sum256([]byte(root))
	applyUnlock, err := lease.Lock(filepath.Join(filepath.Dir(dir), "apply", hex.EncodeToString(digest[:])+".lock"))
	if err != nil {
		return
	}
	defer applyUnlock()
	jobLock, err := AcquireOwnership(dir, jobID)
	if err != nil {
		return
	}
	defer jobLock.Release()
	job, err := Read(path)
	if err != nil {
		return
	}
	var state, applyState, worktree, currentRepo, snapshot string
	job.Get("state", &state)
	job.Get("applyState", &applyState)
	job.Get("worktreePath", &worktree)
	job.Get("repoRoot", &currentRepo)
	job.Get("snapshot", &snapshot)
	var errorCode string
	job.Get("errorCode", &errorCode)
	if (state != "SUCCEEDED" && !(state == "FAILED" && errorCode == "PATCH_UNAVAILABLE")) || applyState == "applied" || currentRepo != repo || worktree == "" || snapshot == "" {
		return
	}
	if info, statErr := os.Stat(worktree); statErr != nil || !info.IsDir() {
		return
	}
	preparation := filepath.Join(stateRoot, "preparations", jobID)
	var revision int64
	job.Get("snapshotRevision", &revision)
	if revision > 0 {
		preparation = filepath.Join(preparation, fmt.Sprintf("revision-%d", revision))
	}
	patchPath := filepath.Join(preparation, "worker.patch")
	patchBytes, patchErr := os.ReadFile(patchPath)
	patchTree := worktree
	preserved := patchErr == nil
	if preserved {
		patchTree = filepath.Join(preparation, "worker-tree")
	} else {
		patch, err := ledger.JobPatch(worktree, snapshot)
		if err != nil {
			return
		}
		patchBytes = []byte(patch)
	}
	prepState := ""
	if data, readErr := os.ReadFile(filepath.Join(preparation, "state.json")); readErr == nil {
		var stateRecord struct {
			State string `json:"state"`
		}
		if json.Unmarshal(data, &stateRecord) != nil {
			return
		}
		prepState = stateRecord.State
	}
	if (prepState == "PREPARING" || (applyState != "" && applyState != "applied") || (state == "FAILED" && errorCode != "PATCH_UNAVAILABLE")) && !preserved {
		return
	}
	paths, err := ledger.JobPatchPaths(root, string(patchBytes))
	if err != nil || len(paths) == 0 {
		return
	}
	for _, path := range paths {
		if !ledger.SameWorktreeContent(patchTree, root, path) {
			return
		}
	}
	persistApplied(stateRoot, dir, jobID, job, map[string]any{"applied": true, "paths": paths}, nil, ApplyOptions{}, "hand_applied_detected")
}

// BindQueuedBaseline binds fresh review evidence once the async index exists and
// the queued scope lease has been promoted. A missing index or inactive lease
// returns false so the host can keep waiting as the Python worker does.
func BindQueuedBaseline(runtimeRoot, jobID string) (bool, error) {
	indexPath := filepath.Join(runtimeRoot, "async_jobs", jobID+".json")
	indexBytes, err := os.ReadFile(indexPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var index map[string]any
	if err = json.Unmarshal(indexBytes, &index); err != nil {
		return false, err
	}
	call, callOK := index["call"].(string)
	callKey, keyOK := index["call_key"].(string)
	projectKey, projectOK := index["project_key"].(string)
	if index["jobId"] != jobID || !callOK || !keyOK || !projectOK ||
		hexHash(call) != jobID {
		return false, errors.New("queued job ownership is unavailable")
	}
	pendingPath := filepath.Join(runtimeRoot, "pending", callKey+".json")
	pendingBytes, err := os.ReadFile(pendingPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var pending map[string]any
	if err = json.Unmarshal(pendingBytes, &pending); err != nil {
		return false, err
	}
	owner, ownerOK := pending["lease_owner"].(string)
	pendingProject, pendingProjectOK := pending["project_key"].(string)
	if pending["job_id"] != jobID || pending["async_call"] != call || !ownerOK ||
		!pendingProjectOK || pendingProject != projectKey || owner != callKey || pending["lease_queued"] != true {
		return false, errors.New("queued job ownership is unavailable")
	}
	scope, scopeOK := stringSlice(pending["scope"])
	if !scopeOK {
		return false, errors.New("queued job ownership is unavailable")
	}
	projectRoot := filepath.Join(runtimeRoot, "projects", projectKey)
	ReleaseHandAppliedBlockers(runtimeRoot, projectRoot, scope)
	state, err := lease.New(projectRoot).OwnerState(owner, float64(time.Now().UnixNano())/1e9)
	if err != nil {
		return false, err
	}
	if state != "ACTIVE" {
		return false, nil
	}
	repoRoot, repoOK := pending["repo_root"].(string)
	taskID, taskOK := pending["task_id"].(string)
	if !repoOK || !taskOK {
		return false, errors.New("queued job ownership is unavailable")
	}
	baseline := ledger.CaptureBaseline(repoRoot, scope, 3)
	if baseline["status"] != ledger.OK {
		return false, fmt.Errorf("%v: %v", baseline["code"], baseline["message"])
	}
	sessionPath, err := ledger.PathForTask(projectRoot, taskID)
	if err != nil {
		return false, err
	}
	loaded := ledger.Load(sessionPath, taskID)
	if loaded.Status != ledger.OK {
		return false, errors.New("queued session record is unavailable")
	}
	loaded.Record["pending_call_baseline"] = baseline
	if saved := ledger.Save(projectRoot, loaded.Record); saved.Status != ledger.OK {
		return false, errors.New("admission baseline could not be bound")
	}
	pendingRecord, err := Decode(pendingBytes)
	if err != nil {
		return false, err
	}
	if err = pendingRecord.Set("baseline", baseline); err != nil {
		return false, err
	}
	if err = Write(pendingPath, pendingRecord); err != nil {
		return false, err
	}
	return true, nil
}

// WaitForQueuedBaseline retries while the index is being created or the lease
// is still queued. The caller's job deadline should bound ctx.
func WaitForQueuedBaseline(ctx context.Context, runtimeRoot, jobID string, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ready, err := BindQueuedBaseline(runtimeRoot, jobID)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func hexHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func stringSlice(value any) ([]string, bool) {
	switch items := value.(type) {
	case []string:
		return items, true
	case []any:
		result := make([]string, len(items))
		for i, item := range items {
			value, ok := item.(string)
			if !ok {
				return nil, false
			}
			result[i] = value
		}
		return result, true
	default:
		return nil, false
	}
}
