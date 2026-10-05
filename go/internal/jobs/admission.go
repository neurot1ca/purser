package jobs

import (
	"context"
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
	projectRoot := filepath.Join(runtimeRoot, "projects", projectKey)
	state, err := lease.New(projectRoot).OwnerState(owner, float64(time.Now().UnixNano())/1e9)
	if err != nil {
		return false, err
	}
	if state != "ACTIVE" {
		return false, nil
	}
	repoRoot, repoOK := pending["repo_root"].(string)
	taskID, taskOK := pending["task_id"].(string)
	scope, scopeOK := stringSlice(pending["scope"])
	if !repoOK || !taskOK || !scopeOK {
		return false, errors.New("queued job ownership is unavailable")
	}
	baseline := ledger.CaptureBaseline(repoRoot, scope, 3)
	if baseline["status"] != ledger.OK {
		return false, errors.New("admission baseline is unavailable")
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
