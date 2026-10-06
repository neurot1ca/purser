package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func TestApplyPythonOracle(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_apply", "oracle.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string         `json:"name"`
			Setup    map[string]any `json:"setup"`
			Snapshot string         `json:"snapshot"`
			Reviewed any            `json:"reviewedSnapshot"`
			Result   map[string]any `json:"result"`
			Record   map[string]any `json:"record"`
			Checkout string         `json:"checkout"`
			Index    map[string]any `json:"index"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			base := t.TempDir()
			repo := filepath.Join(base, "repo")
			state := filepath.Join(base, "state")
			worktree := filepath.Join(base, "worktree")
			if err := os.Mkdir(repo, 0700); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "init", "-q")
			write(t, filepath.Join(repo, "tracked.txt"), "base\n")
			git(t, repo, "add", "--all")
			git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
			git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
			snapshot, err := ledger.CheckoutSnapshot(worktree)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != c.Snapshot {
				t.Fatalf("snapshot got %q want %q", snapshot, c.Snapshot)
			}
			write(t, filepath.Join(worktree, "tracked.txt"), c.Setup["worktree"].(string))
			write(t, filepath.Join(repo, "tracked.txt"), c.Setup["repo"].(string))
			jobID := sha256.Sum256([]byte(c.Name))
			id := hex.EncodeToString(jobID[:])
			job := map[string]any{"jobId": id, "state": c.Setup["state"], "repoRoot": repo, "worktreePath": worktree, "snapshot": snapshot}
			if c.Reviewed != nil {
				job["reviewedSnapshot"] = c.Reviewed
				job["snapshotRevision"] = float64(2)
			}
			jobDir := filepath.Join(state, "codex_exec_mcp", "jobs")
			if err := os.MkdirAll(jobDir, 0700); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(job)
			write(t, filepath.Join(jobDir, id+".json"), string(encoded))
			if c.Reviewed != nil {
				index := filepath.Join(state, "async_jobs", id+".json")
				if err := os.MkdirAll(filepath.Dir(index), 0700); err != nil {
					t.Fatal(err)
				}
				write(t, index, `{"stage":"APPLY"}`)
			}
			result := Apply(state, map[string]any{"jobId": id})
			if c.Setup["repeat"] == true {
				result = Apply(state, map[string]any{"jobId": id})
			}
			var got map[string]any
			rb, _ := json.Marshal(result["structuredContent"])
			_ = json.Unmarshal(rb, &got)
			if got["worktreePath"] != nil {
				got["worktreePath"] = "WORKTREE"
			}
			if !reflect.DeepEqual(got, c.Result) {
				t.Errorf("result got %#v want %#v", got, c.Result)
			}
			stored, err := os.ReadFile(filepath.Join(jobDir, id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(stored, &record); err != nil {
				t.Fatal(err)
			}
			delete(record, "appliedAt")
			record["repoRoot"] = "REPO"
			record["worktreePath"] = "WORKTREE"
			if apply, ok := record["applyResult"].(map[string]any); ok && apply["worktreePath"] != nil {
				apply["worktreePath"] = "WORKTREE"
			}
			if !reflect.DeepEqual(record, c.Record) {
				t.Errorf("record got %#v want %#v", record, c.Record)
			}
			checkout, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.ReplaceAll(string(checkout), "\r\n", "\n") != c.Checkout {
				t.Errorf("checkout got %q want %q", checkout, c.Checkout)
			}
			if c.Index != nil {
				index, err := os.ReadFile(filepath.Join(state, "async_jobs", id+".json"))
				if err != nil {
					t.Fatal(err)
				}
				var gotIndex map[string]any
				_ = json.Unmarshal(index, &gotIndex)
				if !reflect.DeepEqual(gotIndex, c.Index) {
					t.Errorf("index got %#v want %#v", gotIndex, c.Index)
				}
			}
		})
	}
}

func TestApplyCachedRepeatRecordsDecisionAndReleasesLease(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	worktree := filepath.Join(base, "worktree")
	state := filepath.Join(base, "state")
	jobID := "cached-job"
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(state, "codex_exec_mcp", "jobs")
	job := map[string]any{
		"jobId": jobID, "state": "SUCCEEDED", "taskId": "task-1",
		"repoRoot": repo, "worktreePath": worktree, "applyState": "applied",
		"applyResult": map[string]any{"applied": true, "paths": []any{"tracked.txt"}},
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(jobDir, jobID+".json"), string(encoded))

	var decisions, releases int
	options := ApplyOptions{
		RecordDecision: func(got map[string]any, decision, kind, sourceJobID string) {
			decisions++
			if got["taskId"] != "task-1" || decision != "accepted" || kind != "apply" || sourceJobID != jobID {
				t.Errorf("unexpected decision: job=%#v decision=%q kind=%q sourceJobID=%q", got, decision, kind, sourceJobID)
			}
		},
		ReleaseScopeLease: func(gotState, gotJobID string) bool {
			releases++
			if gotState != state || gotJobID != jobID {
				t.Errorf("unexpected lease release: state=%q jobID=%q", gotState, gotJobID)
			}
			return true
		},
	}
	result := Apply(state, map[string]any{"jobId": jobID}, options)
	if result["structuredContent"] == nil {
		t.Fatalf("missing cached result: %#v", result)
	}
	if decisions != 1 || releases != 1 {
		t.Fatalf("side effects: decision calls=%d lease releases=%d; want 1 each", decisions, releases)
	}
}

func TestApplyCachedRepeatReleasesLeaseAfterWorktreeRemoval(t *testing.T) {
	base := t.TempDir()
	state, repo, worktree := filepath.Join(base, "state"), filepath.Join(base, "repo"), filepath.Join(base, "worktree")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	jobID := strings.Repeat("c", 64)
	project, owner := "0123456789abcdef", strings.Repeat("e", 64)
	store := lease.New(filepath.Join(state, "projects", project))
	if got := store.Acquire(owner, "apply-task", []string{"src"}, lease.AcquireOptions{JobID: &jobID}, 100); got.Status != "OK" {
		t.Fatalf("seed active scope lease: %#v", got)
	}
	job := map[string]any{
		"jobId": jobID, "state": "SUCCEEDED", "repoRoot": repo,
		"worktreePath": worktree, "applyState": "applied",
		"applyResult": map[string]any{"applied": true, "paths": []any{"tracked.txt"}},
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), string(encoded))
	index, err := json.Marshal(map[string]any{"jobId": jobID, "project_key": project, "call_key": owner})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "async_jobs", jobID+".json"), string(index))
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}

	result := Apply(state, map[string]any{"jobId": jobID})
	if result["structuredContent"] == nil {
		t.Fatalf("missing cached result: %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(state, "projects", project, "scope_leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	owners := value["owners"].(map[string]any)
	if got := owners[owner].(map[string]any)["state"]; got != "RELEASED" {
		t.Fatalf("scope lease state = %v, want RELEASED", got)
	}
}

func TestApplyFreshSuccessReleasesActiveScopeLease(t *testing.T) {
	base := t.TempDir()
	state, repo, worktree := filepath.Join(base, "state"), filepath.Join(base, "repo"), filepath.Join(base, "worktree")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "tracked.txt"), "base\n")
	git(t, repo, "add", "--all")
	git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	snapshot, err := ledger.CheckoutSnapshot(worktree)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(worktree, "tracked.txt"), "worker change\n")
	jobID := strings.Repeat("b", 64)
	project, owner := "0123456789abcdef", strings.Repeat("f", 64)
	store := lease.New(filepath.Join(state, "projects", project))
	if got := store.Acquire(owner, "apply-task", []string{"src"}, lease.AcquireOptions{JobID: &jobID}, 100); got.Status != "OK" {
		t.Fatalf("seed active scope lease: %#v", got)
	}
	index, err := json.Marshal(map[string]any{"jobId": jobID, "project_key": project, "call_key": owner})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "async_jobs", jobID+".json"), string(index))
	job := map[string]any{
		"jobId": jobID, "state": "SUCCEEDED", "repoRoot": repo,
		"worktreePath": worktree, "snapshot": snapshot,
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), string(encoded))
	data, err := os.ReadFile(filepath.Join(state, "projects", project, "scope_leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	owners := value["owners"].(map[string]any)
	if got := owners[owner].(map[string]any)["state"]; got != "ACTIVE" {
		t.Fatalf("seed scope lease state = %v, want ACTIVE", got)
	}

	result := Apply(state, map[string]any{"jobId": jobID})
	if structured, _ := result["structuredContent"].(map[string]any); structured["applied"] != true {
		t.Fatalf("fresh apply failed: %#v", result)
	}
	data, err = os.ReadFile(filepath.Join(state, "projects", project, "scope_leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	owners = value["owners"].(map[string]any)
	if got := owners[owner].(map[string]any)["state"]; got != "RELEASED" {
		t.Fatalf("scope lease state = %v, want RELEASED", got)
	}
}

func TestApplyFreshSuccessPersistsReviewerDecisionCountsOnce(t *testing.T) {
	base := t.TempDir()
	state, repo, worktree := filepath.Join(base, "state"), filepath.Join(base, "repo"), filepath.Join(base, "worktree")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "tracked.txt"), "base\n")
	git(t, repo, "add", "--all")
	git(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	snapshot, err := ledger.CheckoutSnapshot(worktree)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(worktree, "tracked.txt"), "worker change\n")
	jobID := strings.Repeat("a", 64)
	job := map[string]any{
		"jobId": jobID, "state": "SUCCEEDED", "taskId": "decision-task", "jobPhase": "implementation",
		"repoRoot": repo, "worktreePath": worktree, "snapshot": snapshot,
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"), string(encoded))
	args := map[string]any{"jobId": jobID, "reviewer1Valid": json.Number("3"), "reviewer1False": json.Number("1")}
	result := Apply(state, args)
	structured, _ := result["structuredContent"].(map[string]any)
	if structured["applied"] != true {
		t.Fatalf("fresh apply failed: %#v", result)
	}
	eventPath := filepath.Join(state, "telemetry", "events", "by_task", "decision-task.jsonl")
	countDecisionEvents := func(data []byte) int {
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			if event["event_type"] == "CLAUDE_DECISION_RECORDED" {
				count++
			}
		}
		return count
	}
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if got := countDecisionEvents(data); got != 1 {
		t.Fatalf("fresh apply emitted %d decision events, want 1: %s", got, data)
	}
	var event map[string]any
	for _, line := range lines {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			t.Fatal(err)
		}
		if candidate["event_type"] == "CLAUDE_DECISION_RECORDED" {
			event = candidate
			break
		}
	}
	payload, _ := event["payload"].(map[string]any)
	if payload["reviewer1_valid_count"] != float64(3) || payload["reviewer1_false_count"] != float64(1) {
		t.Fatalf("reviewer counts were not persisted from fresh codex_apply: %#v", payload)
	}
	result = Apply(state, args)
	if structured, _ := result["structuredContent"].(map[string]any); structured["applied"] != true {
		t.Fatalf("replayed apply failed: %#v", result)
	}
	data, err = os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := countDecisionEvents(data); got != 2 {
		t.Fatalf("Python-compatible replay decision count = %d, want 2", got)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE=2000-01-01T00:00:00+00:00")
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
}
func write(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
