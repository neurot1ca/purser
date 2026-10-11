package jobs

import (
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

type admissionFixture struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Pending any    `json:"pending"`
	Session any    `json:"session"`
	Leases  any    `json:"leases"`
}

func TestAdmissionFixtureReplay(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_queued_baseline", "admission.json")
	wantBytes, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("missing admission oracle fixture %s (regenerate it from the source project): %v", fixturePath, err)
	}
	var want []admissionFixture
	if err = json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != 5 {
		t.Fatalf("fixture has %d cases; want 5", len(want))
	}
	for _, tc := range want {
		t.Run(tc.Name, func(t *testing.T) {
			root := admissionTempDir(t)
			setupAdmission(t, root, tc.Name)
			ready, err := BindQueuedBaseline(root, admissionJobID())
			if err != nil {
				t.Fatal(err)
			}
			if ready != tc.Ready {
				t.Fatalf("ready=%v want %v", ready, tc.Ready)
			}
			got := admissionFixture{Name: tc.Name, Ready: ready,
				Pending: admissionJSON(t, filepath.Join(root, "pending", admissionOwner()+".json")),
				Session: admissionJSON(t, admissionSessionPath(t, root)),
				Leases:  admissionJSON(t, filepath.Join(root, "projects", "project", "scope_leases.json"))}
			got.Pending = normalizeAdmissionPaths(got.Pending, root)
			if !reflect.DeepEqual(got, tc) {
				t.Fatalf("Go records differ\n got: %#v\nwant: %#v", got, tc)
			}
		})
	}
}

func setupAdmission(t *testing.T, root, name string) {
	t.Helper()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "file"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "src/file")
	git("commit", "-qm", "baseline")
	call, owner, jobID := "queued-call", admissionOwner(), admissionJobID()
	if err := os.MkdirAll(filepath.Join(root, "codex_exec_mcp", "jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeFixtureJSON(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"), map[string]any{"jobId": jobID, "call": call, "state": "QUEUED"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "projects", "project"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pending"), 0700); err != nil {
		t.Fatal(err)
	}
	if name != "missing_job" {
		if name == "lease_not_held" {
			blocker := "blocker-job"
			lease.New(filepath.Join(root, "projects", "project")).Acquire("blocker", "blocker-task", []string{"src"}, lease.AcquireOptions{JobID: &blocker}, 1800000000)
		}
		acquired := lease.New(filepath.Join(root, "projects", "project")).Acquire(owner, "task", []string{"src"}, lease.AcquireOptions{JobID: &jobID, QueueConflict: true}, 1800000000)
		if acquired.Status != "OK" {
			t.Fatalf("lease setup: %+v", acquired)
		}
		pending := map[string]any{"job_id": jobID, "async_call": call, "lease_queued": true, "lease_owner": owner,
			"project_key": "project", "repo_root": repo, "task_id": "task", "scope": []string{"src"}}
		if err := writeFixtureJSON(filepath.Join(root, "pending", owner+".json"), pending); err != nil {
			t.Fatal(err)
		}
		if name != "index_not_ready" {
			if err := os.MkdirAll(filepath.Join(root, "async_jobs"), 0700); err != nil {
				t.Fatal(err)
			}
			index := map[string]any{"jobId": jobID, "call": call, "call_key": owner, "project_key": "project"}
			if err := writeFixtureJSON(filepath.Join(root, "async_jobs", jobID+".json"), index); err != nil {
				t.Fatal(err)
			}
		}
		baseline := map[string]any{"status": "UNKNOWN", "code": "QUEUED"}
		if name == "already_bound" {
			baseline = ledger.CaptureBaseline(repo, []string{"src"}, 0)
			pending["baseline"] = baseline
		}
		if saved := ledger.Save(filepath.Join(root, "projects", "project"), ledger.BuildSessionRecord("task", nil, "", "", "", map[string]any{"pending_call_baseline": baseline})); saved.Status != ledger.OK {
			t.Fatal("session setup failed")
		}
	}
}

func normalizeAdmissionPaths(v any, root string) any {
	switch x := v.(type) {
	case string:
		escapedRoot := strings.ReplaceAll(root, `\`, `\\`)
		x = strings.ReplaceAll(x, escapedRoot, "__ROOT__")
		x = strings.ReplaceAll(x, root, "__ROOT__")
		if strings.Contains(x, "__ROOT__") {
			x = strings.ReplaceAll(x, `\\`, "/")
			x = strings.ReplaceAll(x, `\`, "/")
		}
		return x
	case map[string]any:
		for key, child := range x {
			x[key] = normalizeAdmissionPaths(child, root)
		}
	case []any:
		for i := range x {
			x[i] = normalizeAdmissionPaths(x[i], root)
		}
	}
	return v
}

func admissionJSON(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err = json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "scope_leases") {
		out = normalizeAdmissionTimes(out)
	}
	return out
}
func normalizeAdmissionTimes(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if strings.HasSuffix(k, "_at") {
				x[k] = "__TIME__"
			} else {
				x[k] = normalizeAdmissionTimes(child)
			}
		}
	case []any:
		for i := range x {
			x[i] = normalizeAdmissionTimes(x[i])
		}
	}
	return v
}
func admissionSessionPath(t *testing.T, root string) string {
	t.Helper()
	path, err := ledger.PathForTask(filepath.Join(root, "projects", "project"), "task")
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func admissionOwner() string { return "owner-key" }
func admissionJobID() string { return hexHash("queued-call") }
func admissionTempDir(t *testing.T) string {
	t.Helper()
	base := filepath.Join("..", "..", "..", ".tmp", "go-admission")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "case-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	absolute, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}
func writeFixtureJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
