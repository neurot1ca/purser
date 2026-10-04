package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"unicode/utf8"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

// PendingAdmission carries the already-built synchronous pending and session
// values through Python Pre's reconciliation, lease, and persistence sequence.
type PendingAdmission struct {
	RuntimeRoot, ProjectKey, OwnerKey, TaskID string
	Pending, Session                          map[string]any
	PendingPath                               string
	Scope                                     []string
	JobID                                     *string
	TimeoutSeconds, Now                       float64
	ReadOnly, AllowOverlap, HotPathQueue      bool
	ThreadID, Repository                      *string
}

type AdmissionResult struct {
	Status, Code string
	Queued       bool
	Pending      map[string]any
}

// AdmitPending mirrors Python's sync-reachable rejected-start reconciliation,
// lease admission, then pending -> session -> thread-index writes.
func AdmitPending(in PendingAdmission) AdmissionResult {
	result := AdmissionResult{Status: "OK", Code: "LEASE_ACQUIRED", Pending: in.Pending}
	projectRoot := filepath.Join(in.RuntimeRoot, "projects", in.ProjectKey)
	leased := false
	if in.Repository != nil {
		reconcileRejectedStarts(in.RuntimeRoot, projectRoot, in.ProjectKey, in.Now)
		admitted := lease.New(projectRoot).Acquire(in.OwnerKey, in.TaskID, in.Scope, lease.AcquireOptions{
			JobID: in.JobID, TimeoutSeconds: in.TimeoutSeconds, ReadOnly: in.ReadOnly,
			AllowOverlap: in.AllowOverlap, HotPathQueue: in.HotPathQueue,
		}, in.Now)
		result.Status, result.Code = admitted.Status, admitted.Code
		if admitted.Status != "OK" {
			return result
		}
		leased = true
		in.Pending["lease_owner"] = in.OwnerKey
		if admitted.Code == "LEASE_QUEUED" {
			in.Pending["lease_queued"] = true
			queued := map[string]any{"status": "UNKNOWN", "code": "QUEUED"}
			in.Pending["baseline"], in.Session["pending_call_baseline"] = queued, queued
			result.Queued = true
		}
	}
	result.Pending = in.Pending
	if err := atomicAdmissionJSON(in.PendingPath, in.Pending); err != nil {
		result.Status, result.Code = "UNKNOWN", "PENDING_WRITE_FAILED"
		if leased {
			_ = lease.New(projectRoot).Release(in.OwnerKey, in.Now)
		}
		return result
	}
	if saved := ledger.Save(projectRoot, in.Session); saved.Status != ledger.OK {
		result.Status, result.Code = "UNKNOWN", saved.Code
		if leased {
			_ = lease.New(projectRoot).Release(in.OwnerKey, in.Now)
		}
		_ = os.Remove(in.PendingPath)
		return result
	}
	if in.ThreadID != nil && in.Repository != nil {
		path := threadIndexFile(in.RuntimeRoot, *in.ThreadID)
		if path != "" {
			if err := atomicAdmissionJSON(path, map[string]any{"thread_id": *in.ThreadID, "project_root": *in.Repository}); err != nil {
				result.Status, result.Code = "UNKNOWN", "THREAD_INDEX_WRITE_FAILED"
				if leased {
					_ = lease.New(projectRoot).Release(in.OwnerKey, in.Now)
				}
				_ = os.Remove(in.PendingPath)
				return result
			}
		}
	}
	return result
}

func reconcileRejectedStarts(root, projectRoot, projectKey string, now float64) {
	paths, _ := filepath.Glob(filepath.Join(root, "pending", "*.json"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		var pending map[string]any
		if err != nil || !utf8.Valid(raw) || json.Unmarshal(raw, &pending) != nil || pending["start_rejected"] != true || pending["project_key"] != projectKey {
			continue
		}
		call, ok := pending["async_call"].(string)
		if !ok {
			continue
		}
		jobID := sha256.Sum256([]byte(call))
		id := hex.EncodeToString(jobID[:])
		jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", id+".json")
		jobRaw, readErr := os.ReadFile(jobPath)
		terminal := errors.Is(readErr, os.ErrNotExist)
		if readErr == nil && utf8.Valid(jobRaw) {
			var job map[string]any
			if json.Unmarshal(jobRaw, &job) == nil && job["jobId"] == id && job["call"] == call {
				state, _ := job["state"].(string)
				terminal = state == "SUCCEEDED" || state == "FAILED" || state == "CANCELLED" || state == "TIMED_OUT"
			}
		}
		if !terminal {
			continue
		}
		if owner, ok := pending["lease_owner"].(string); ok {
			_ = lease.New(projectRoot).Release(owner, now)
		}
		_ = os.Remove(path)
	}
}

func atomicAdmissionJSON(path string, value any) error {
	data, err := canonicalJSON(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), path)
}

func threadIndexFile(root, threadID string) string {
	path, ok := threadIndexPath(root, threadID)
	if !ok {
		return ""
	}
	return path
}
