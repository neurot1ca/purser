package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func TestPendingAdmissionP47Fixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/pending_admission.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string `json:"name"`
			Setup    string `json:"setup"`
			Expected struct {
				Status       string `json:"status"`
				Code         string `json:"code"`
				Queued       bool   `json:"queued"`
				Pending      bool   `json:"pending"`
				Session      bool   `json:"session"`
				ThreadIndex  bool   `json:"thread_index"`
				StalePending bool   `json:"stale_pending"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "codex._reconcile_rejected_starts+ledger.ScopeLeases.acquire+pending/session/thread-index writes" {
		t.Fatalf("fixture: %v %q", err, fixture.Oracle)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			if len(item.Name) < 4 || item.Name[:4] != "p47_" {
				t.Fatalf("case is not p47: %s", item.Name)
			}
			root := t.TempDir()
			projectRoot := filepath.Join(root, "projects", "project-p47")
			_ = os.MkdirAll(filepath.Join(root, "pending"), 0700)
			if item.Setup == "fail_pending" {
				_ = os.Remove(filepath.Join(root, "pending"))
				_ = os.WriteFile(filepath.Join(root, "pending"), []byte("blocked"), 0600)
			}
			if item.Setup == "fail_session" {
				_ = os.MkdirAll(projectRoot, 0700)
				_ = os.WriteFile(filepath.Join(projectRoot, "sessions"), []byte("blocked"), 0600)
			}
			if item.Setup == "fail_index" {
				_ = os.WriteFile(filepath.Join(root, "threads"), []byte("blocked"), 0600)
			}
			if item.Setup == "lease_uncertain" {
				_ = os.MkdirAll(projectRoot, 0700)
				_ = os.WriteFile(filepath.Join(projectRoot, "scope_leases.json"), []byte("{"), 0600)
			}
			if item.Setup == "conflict" || item.Setup == "stale" {
				store := lease.New(projectRoot)
				jobCall := "call-old"
				digest := sha256.Sum256([]byte(jobCall))
				jobID := hex.EncodeToString(digest[:])
				owners := map[string]any{"oldowner": map[string]any{"task_id": "old-task", "scope": []any{"src"}, "state": "ACTIVE", "job_id": jobID, "acquired_at": float64(900), "queued_at": float64(900)}}
				if item.Setup == "conflict" {
					owners["other"] = owners["oldowner"]
				}
				encoded, _ := json.Marshal(map[string]any{"version": 1, "owners": owners})
				_ = os.MkdirAll(projectRoot, 0700)
				_ = os.WriteFile(filepath.Join(projectRoot, "scope_leases.json"), encoded, 0600)
				if item.Setup == "stale" {
					old := map[string]any{"start_rejected": true, "project_key": "project-p47", "async_call": jobCall, "lease_owner": "oldowner"}
					b, _ := json.Marshal(old)
					_ = os.WriteFile(filepath.Join(root, "pending", "stale.json"), b, 0600)
				}
				_ = store
			}
			if item.Setup == "queued" {
				_ = os.MkdirAll(projectRoot, 0700)
				encoded, _ := json.Marshal(map[string]any{"version": 1, "owners": map[string]any{
					"owner-p47": map[string]any{"task_id": "task-p47", "scope": []any{"src"}, "state": "QUEUED", "job_id": "job-p47", "queued_at": float64(900)},
					"blocker":   map[string]any{"task_id": "blocking-task", "scope": []any{"src"}, "state": "ACTIVE", "job_id": "blocker-job", "acquired_at": float64(900), "queued_at": float64(900)},
				}})
				_ = os.WriteFile(filepath.Join(projectRoot, "scope_leases.json"), encoded, 0600)
			}
			scope := []string{"src"}
			readOnly := item.Setup == "plan"
			if readOnly {
				scope = []string{}
			}
			baseline := map[string]any{"status": "OK", "head": "base"}
			pendingPath := filepath.Join(root, "pending", "current.json")
			pending := map[string]any{"task_id": "task-p47", "project_key": "project-p47", "phase": "implementation", "baseline": baseline}
			if readOnly {
				pending["phase"] = "plan"
			}
			var repository *string
			repo := filepath.Join(root, "repo")
			if item.Setup != "unresolved" {
				_ = os.MkdirAll(repo, 0700)
				repository = &repo
			}
			thread := "thread-p47"
			var threadID *string = &thread
			if repository == nil {
				threadID = nil
			}
			session := ledger.BuildSessionRecord("task-p47", threadID, "operation", "model", "high", map[string]any{"pending_call_baseline": baseline, "phase": pending["phase"], "scope": scope, "task_contract_grounded": true, "task_objective": "fixture objective", "acceptance_criteria": "fixture acceptance"})
			got := AdmitPending(PendingAdmission{RuntimeRoot: root, ProjectKey: "project-p47", OwnerKey: "owner-p47", TaskID: "task-p47", Pending: pending, Session: session, PendingPath: pendingPath, Scope: scope, ReadOnly: readOnly, Now: 1000, Repository: repository, ThreadID: threadID})
			if got.Status != item.Expected.Status || got.Code != item.Expected.Code || got.Queued != item.Expected.Queued {
				t.Fatalf("result = %#v, want %s/%s queued=%v", got, item.Expected.Status, item.Expected.Code, item.Expected.Queued)
			}
			checkExists(t, pendingPath, item.Expected.Pending)
			checkExists(t, filepath.Join(projectRoot, "sessions", mustSessionName("task-p47")), item.Expected.Session)
			index := threadIndexFile(root, "thread-p47")
			if index == "" {
				t.Fatal("invalid test thread")
			}
			checkExists(t, index, item.Expected.ThreadIndex)
			if item.Setup == "stale" {
				checkExists(t, filepath.Join(root, "pending", "stale.json"), item.Expected.StalePending)
			}
		})
	}
}

func mustSessionName(task string) string {
	path, _ := ledger.PathForTask("", task)
	return filepath.Base(path)
}
func checkExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	got := err == nil
	if got != want {
		t.Fatalf("exists(%s)=%v, want %v (err %v)", path, got, want, err)
	}
}
