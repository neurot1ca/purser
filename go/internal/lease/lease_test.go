package lease

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestPythonLeaseRecords(t *testing.T) {
	valid := `{"version":1,"owners":{"active":{"state":"ACTIVE","scope":["src"],"task_id":"task"},"queued":{"state":"QUEUED","scope":["tests"],"task_id":"task2","job_id":"job","queued_at":1.5}},"conflicts":[{"path":"src/file","recorded_at":2}]}`
	if err := Validate([]byte(valid)); err != nil {
		t.Fatalf("valid Python-shaped record rejected: %v", err)
	}
	for _, tc := range []struct{ name, raw string }{
		{"owner-state", `{"version":1,"owners":{"x":{"state":"BROKEN","scope":["a"],"task_id":"t"}}}`},
		{"queued-time", `{"version":1,"owners":{"x":{"state":"QUEUED","scope":["a"],"task_id":"t","job_id":"j","queued_at":"later"}}}`},
		{"empty-scope", `{"version":1,"owners":{"x":{"state":"ACTIVE","scope":[],"task_id":"t"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate([]byte(tc.raw)); err == nil {
				t.Fatal("malformed lease record accepted")
			}
		})
	}
}

func TestAdmissionHelper(t *testing.T) {
	root := os.Getenv("PURSER_LEASE_ADMISSION_ROOT")
	if root == "" {
		t.Skip("parity helper")
	}
	got := New(root).Acquire("owner", "task", []string{"src"}, AcquireOptions{}, 100)
	if got.Status != "OK" || got.Code != "LEASE_ACQUIRED" {
		t.Fatalf("free scope admission: %#v", got)
	}
}

func TestAdmissionReleasesStaleJobLease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		jobState string
		missing  bool
		seedNow  float64
		retryNow float64
	}{
		{name: "missing_job", missing: true, seedNow: 100, retryNow: 100 + missingJobOrphanSeconds + 1},
		{name: "terminal_job_worktree_gone", jobState: "SUCCEEDED", seedNow: 100, retryNow: 101},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			store := New(filepath.Join(root, "projects", "project"))
			call := "terminal fixture"
			jobID := fmt.Sprintf("%x", sha256.Sum256([]byte(call)))
			if !tc.missing {
				jobs := filepath.Join(root, "codex_exec_mcp", "jobs")
				if err := os.MkdirAll(jobs, 0700); err != nil {
					t.Fatal(err)
				}
				job, err := json.Marshal(map[string]any{"jobId": jobID, "call": call, "state": tc.jobState,
					"worktreePath": filepath.Join(root, "removed-worktree")})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(jobs, jobID+".json"), job, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := store.Acquire("stale", "old-task", []string{"src"}, AcquireOptions{JobID: &jobID}, tc.seedNow); got.Status != "OK" {
				t.Fatalf("seed stale lease: %#v", got)
			}
			got := store.Acquire("next", "next-task", []string{"src"}, AcquireOptions{}, tc.retryNow)
			if got.Status != "OK" || got.Code != "LEASE_ACQUIRED" {
				t.Fatalf("stale owner blocked admission: %#v", got)
			}
			value, err := store.read()
			if err != nil {
				t.Fatal(err)
			}
			owners := value["owners"].(map[string]any)
			if owners["stale"].(map[string]any)["state"] != "RELEASED" || owners["next"].(map[string]any)["state"] != "ACTIVE" {
				t.Fatalf("stale lease was not reconciled: %#v", owners)
			}
		})
	}
}

func TestAdmissionKeepsRecentMissingJobLease(t *testing.T) {
	root := t.TempDir()
	store := New(filepath.Join(root, "projects", "project"))
	call := "recent missing fixture"
	jobID := fmt.Sprintf("%x", sha256.Sum256([]byte(call)))
	nextCall := "next fixture"
	nextJobID := fmt.Sprintf("%x", sha256.Sum256([]byte(nextCall)))
	if got := store.Acquire("recent", "recent-task", []string{"src"}, AcquireOptions{JobID: &jobID}, 100); got.Status != "OK" {
		t.Fatalf("seed recent lease: %#v", got)
	}
	got := store.Acquire("next", "next-task", []string{"src"}, AcquireOptions{
		JobID: &nextJobID, QueueConflict: true,
	}, 101)
	if got.Status != "OK" || got.Code != "LEASE_QUEUED" {
		t.Fatalf("recent missing lease did not block the next admission: %#v", got)
	}
	value, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	owners := value["owners"].(map[string]any)
	if owners["recent"].(map[string]any)["state"] != "ACTIVE" || owners["next"].(map[string]any)["state"] != "QUEUED" {
		t.Fatalf("recent missing lease was reconciled too early: %#v", owners)
	}
}

func TestAcquireQueuesBehindOlderOverlappingQueuedOwner(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	blockerJob, olderJob, newerJob := "blocker", "older", "newer"
	if got := store.Acquire("blocker", "blocker-task", []string{"scope/occupied"}, AcquireOptions{JobID: &blockerJob}, 100); got.Code != "LEASE_ACQUIRED" {
		t.Fatalf("seed blocker: %#v", got)
	}
	if got := store.Acquire("older", "older-task", []string{"scope"}, AcquireOptions{JobID: &olderJob, QueueConflict: true}, 101); got.Code != "LEASE_QUEUED" {
		t.Fatalf("queue older broad owner: %#v", got)
	}
	got := store.Acquire("newer", "newer-task", []string{"scope/free"}, AcquireOptions{JobID: &newerJob, QueueConflict: true, AllowOverlap: true}, 102)
	if got.Status != "OK" || got.Code != "LEASE_QUEUED" {
		t.Fatalf("newer overlapping owner bypassed older queue: %#v", got)
	}
}

func TestAcquireOlderQueuedOwnerOnlyQueuesWhenRequested(t *testing.T) {
	fixture := `{"version":1,"owners":{"active":{"state":"ACTIVE","scope":["scope/live"],"task_id":"active-task","acquired_at":100},"queued":{"state":"QUEUED","scope":["scope"],"task_id":"queued-task","job_id":"queued","queued_at":101}}}`
	for _, test := range []struct {
		name          string
		queueConflict bool
		wantCode      string
	}{
		{name: "non-queueing caller conflicts with active owner", wantCode: "SCOPE_LEASE_CONFLICT"},
		{name: "queueing caller queues", queueConflict: true, wantCode: "LEASE_QUEUED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "scope_leases.json"), []byte(fixture), 0600); err != nil {
				t.Fatal(err)
			}
			jobID := "request"
			got := New(root).Acquire("request", "request-task", []string{"scope/file"}, AcquireOptions{JobID: &jobID, QueueConflict: test.queueConflict}, 102)
			if got.Code != test.wantCode {
				t.Fatalf("Acquire() = %#v, want code %s", got, test.wantCode)
			}
		})
	}
}

func TestAcquireAdmitsWhenOlderQueuedScopeDoesNotOverlap(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	blockerJob, olderJob, newerJob := "blocker", "older", "newer"
	if got := store.Acquire("blocker", "blocker-task", []string{"occupied"}, AcquireOptions{JobID: &blockerJob}, 100); got.Code != "LEASE_ACQUIRED" {
		t.Fatalf("seed blocker: %#v", got)
	}
	if got := store.Acquire("older", "older-task", []string{"occupied"}, AcquireOptions{JobID: &olderJob, QueueConflict: true}, 101); got.Code != "LEASE_QUEUED" {
		t.Fatalf("seed older queued owner: %#v", got)
	}
	got := store.Acquire("newer", "newer-task", []string{"other"}, AcquireOptions{JobID: &newerJob, QueueConflict: true}, 102)
	if got.Status != "OK" || got.Code != "LEASE_ACQUIRED" {
		t.Fatalf("non-overlapping owner was unnecessarily queued: %#v", got)
	}
}

func TestReleasePromotesOverlappingQueuedOwnersInQueueOrder(t *testing.T) {
	root := t.TempDir()
	fixture := `{"version":1,"owners":{"blocker":{"state":"ACTIVE","scope":["scope/occupied"],"task_id":"blocker-task","acquired_at":90},"a-newer":{"state":"QUEUED","scope":["scope/free"],"task_id":"newer-task","job_id":"newer","queued_at":92},"z-older":{"state":"QUEUED","scope":["scope"],"task_id":"older-task","job_id":"older","queued_at":91}}}`
	if err := os.WriteFile(filepath.Join(root, "scope_leases.json"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	store := New(root)
	if got := store.Release("blocker", 100); got.Code != "LEASE_RELEASED" {
		t.Fatalf("release blocker: %#v", got)
	}
	value, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	owners := value["owners"].(map[string]any)
	if owners["z-older"].(map[string]any)["state"] != "ACTIVE" || owners["a-newer"].(map[string]any)["state"] != "QUEUED" {
		t.Fatalf("queue did not promote older overlapping owner first: %#v", owners)
	}
	if got := store.Release("z-older", 101); got.Code != "LEASE_RELEASED" {
		t.Fatalf("release older owner: %#v", got)
	}
	value, err = store.read()
	if err != nil {
		t.Fatal(err)
	}
	owners = value["owners"].(map[string]any)
	if owners["a-newer"].(map[string]any)["state"] != "ACTIVE" {
		t.Fatalf("newer owner was not promoted after older owner finished: %#v", owners)
	}
}

func TestAcquireReportsFirstConflictingOwnerInStoredOrder(t *testing.T) {
	root := t.TempDir()
	fixture := `{"version":1,"owners":{"z-first":{"state":"ACTIVE","scope":["src"],"task_id":"z-task","acquired_at":90},"a-second":{"state":"ACTIVE","scope":["src"],"task_id":"a-task","acquired_at":90},"expired":{"state":"ACTIVE","scope":["old"],"task_id":"expired-task","acquired_at":1,"deadline_at":99}}}`
	if err := os.WriteFile(filepath.Join(root, "scope_leases.json"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	store := New(root)
	for attempt := 0; attempt < 2; attempt++ {
		got := store.Acquire("request", fmt.Sprintf("request-%d", attempt), []string{"src/file"}, AcquireOptions{}, 100)
		if got.Code != "SCOPE_LEASE_CONFLICT" || got.TaskID != "z-task" {
			t.Fatalf("attempt %d reported conflict %#v, want first stored owner z-task", attempt, got)
		}
	}
}

func TestAdmissionParityOutcome(t *testing.T) {
	root := os.Getenv("PURSER_LEASE_ADMISSION_ROOT")
	if root == "" {
		t.Skip("parity helper")
	}
	var fixture struct {
		Cases []json.RawMessage `json:"cases"`
	}
	rawFixture, err := os.ReadFile(os.Getenv("PURSER_LEASE_ADMISSION_FIXTURE"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawFixture, &fixture); err != nil {
		t.Fatal(err)
	}
	var input struct {
		Name      string  `json:"name"`
		Now       float64 `json:"now"`
		FailWrite bool    `json:"fail_write"`
		Requests  []struct {
			OwnerKey string   `json:"owner_key"`
			TaskID   string   `json:"task_id"`
			Scope    []string `json:"scope"`
			Options  struct {
				JobID          *string `json:"job_id"`
				TimeoutSeconds float64 `json:"timeout_seconds"`
				ReadOnly       bool    `json:"read_only"`
				QueueConflict  bool    `json:"queue_conflict"`
				AllowOverlap   bool    `json:"allow_overlap"`
				HotPathQueue   bool    `json:"hot_path_queue"`
			} `json:"options"`
		} `json:"requests"`
	}
	for _, rawCase := range fixture.Cases {
		var candidate struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rawCase, &candidate); err != nil {
			t.Fatal(err)
		}
		if candidate.Name == os.Getenv("PURSER_LEASE_ADMISSION_CASE") {
			if err := json.Unmarshal(rawCase, &input); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if input.Name == "" {
		t.Fatalf("admission case %q missing from fixture", os.Getenv("PURSER_LEASE_ADMISSION_CASE"))
	}
	if input.FailWrite {
		oldReplace := replaceLeaseFile
		replaceLeaseFile = func(string, string) error { return os.ErrPermission }
		t.Cleanup(func() { replaceLeaseFile = oldReplace })
	}
	store := New(root)
	results := make([]AdmissionResult, 0, len(input.Requests))
	for _, request := range input.Requests {
		results = append(results, store.Acquire(request.OwnerKey, request.TaskID, request.Scope,
			AcquireOptions(request.Options), input.Now))
	}
	value, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	owners, err := json.Marshal(value["owners"])
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(map[string]any{"results": results, "owners": json.RawMessage(owners)})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("LEASE_ADMISSION_RESULT:" + string(output))
}

func TestLockContentionAcrossProcesses(t *testing.T) {
	if path := os.Getenv("PURSER_LEASE_LOCK_HELPER"); path != "" {
		release, err := Lock(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".ready", []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
		if err := release(); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "scope_leases.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockContentionAcrossProcesses$")
	cmd.Env = append(os.Environ(), "PURSER_LEASE_LOCK_HELPER="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not acquire lease lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	release, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Fatalf("second process acquired before holder released: %s", elapsed)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child process: %v", err)
	}
}

func TestReleasePromotesQueuedOwnerLikePython(t *testing.T) {
	root := t.TempDir()
	input := `{"version":1,"owners":{"a":{"acquired_at":90,"scope":["src"],"state":"ACTIVE","task_id":"a"},"b":{"job_id":"job","queued_at":91,"scope":["docs"],"state":"QUEUED","task_id":"b"}}}`
	if err := os.WriteFile(filepath.Join(root, "scope_leases.json"), []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	got := New(root).Release("a", 100)
	if got != (Result{"OK", "LEASE_RELEASED"}) {
		t.Fatalf("release result: %#v", got)
	}
	raw, err := os.ReadFile(filepath.Join(root, "scope_leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"conflicts":[],"owners":{"a":{"acquired_at":90,"released_at":100,"scope":["src"],"state":"RELEASED","task_id":"a"},"b":{"acquired_at":100,"job_id":"job","queued_at":91,"scope":["docs"],"state":"ACTIVE","task_id":"b"}},"version":1}`
	if string(raw) != want {
		t.Fatalf("released Python fixture mismatch:\n got %s\nwant %s", raw, want)
	}
	if got := New(root).Release("b", 101); got.Code != "LEASE_RELEASED" {
		t.Fatalf("loaded mutation: %#v", got)
	}
	value, err := New(root).read()
	if err != nil || value["owners"].(map[string]any)["b"].(map[string]any)["released_at"] != json.Number("101") {
		t.Fatalf("mutated persisted lease: %#v, %v", value, err)
	}
}

func TestFailedReleaseWritePreservesPriorRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	prior := `{"version":1,"owners":{"a":{"state":"ACTIVE","scope":["src"],"task_id":"a"}}}`
	if err := os.WriteFile(path, []byte(prior), 0600); err != nil {
		t.Fatal(err)
	}
	oldReplace := replaceLeaseFile
	replaceLeaseFile = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { replaceLeaseFile = oldReplace })
	if got := New(root).Release("a", 100); got.Code != "LEASE_RELEASE_UNCERTAIN" {
		t.Fatalf("failed mutation result: %#v", got)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != prior {
		t.Fatalf("prior record not recoverable: err=%v content=%s", err, after)
	}
}

func TestFailedReconcileWritePreservesPriorRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	prior := `{"version":1,"owners":{"expired":{"state":"ACTIVE","scope":["src"],"task_id":"expired","acquired_at":1,"deadline_at":2}}}`
	if err := os.WriteFile(path, []byte(prior), 0600); err != nil {
		t.Fatal(err)
	}
	oldReplace := replaceLeaseFile
	replaceLeaseFile = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { replaceLeaseFile = oldReplace })
	if _, err := New(root).Reconcile(2); err == nil {
		t.Fatal("failed replacement reported successful reconciliation")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != prior {
		t.Fatalf("prior record not recoverable after failed reconcile: err=%v content=%s", err, after)
	}
}

func TestPromoteThenReleaseReadsFloatTimestampsInMemory(t *testing.T) {
	store := New(t.TempDir())
	value := map[string]any{"owners": map[string]any{
		"a": map[string]any{"state": "ACTIVE", "scope": []any{"src"}, "task_id": "a"},
		"b": map[string]any{"state": "QUEUED", "scope": []any{"docs"}, "task_id": "b", "job_id": "j", "queued_at": json.Number("1")},
	}}
	if !store.promote(value, 2) {
		t.Fatal("queued owner was not promoted")
	}
	if changed, missing := store.releaseOwner(value, "a", 3); !changed || missing {
		t.Fatalf("release: %v %v", changed, missing)
	}
	owners := value["owners"].(map[string]any)
	if n, ok := pythonFloat(owners["b"].(map[string]any)["acquired_at"]); !ok || n != 2 {
		t.Fatalf("promoted float timestamp rejected: %v %v", n, ok)
	}
	if n, ok := pythonFloat(owners["a"].(map[string]any)["released_at"]); !ok || n != 3 {
		t.Fatalf("released float timestamp rejected: %v %v", n, ok)
	}
}

func TestHotPathPromotionHonorsConflictOverlap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	fixture := `{"version":1,"conflicts":[{"path":"src/sub/file","recorded_at":90},{"path":"stale","recorded_at":1}],"owners":{"active":{"state":"ACTIVE","scope":["src/sub"],"task_id":"a","acquired_at":80},"old":{"state":"RELEASED","scope":["old"],"task_id":"old","released_at":1},"queued":{"state":"QUEUED","scope":["src"],"task_id":"b","job_id":"j","queued_at":85,"hot_path_queue":true}}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := New(root).Reconcile(100)
	if err != nil || changed {
		t.Fatalf("blocked hot-path promotion: changed=%v err=%v", changed, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || string(after) != fixture {
		t.Fatalf("no-transition reconcile pruned data: %v data=%s", readErr, after)
	}
}

func TestHotPathPromotionIgnoresActiveScopeOverlapWithoutConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	fixture := `{"version":1,"owners":{"active":{"state":"ACTIVE","scope":["src/sub"],"task_id":"a","acquired_at":80},"queued":{"state":"QUEUED","scope":["src"],"task_id":"b","job_id":"j","queued_at":85,"hot_path_queue":true}}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := New(root).Reconcile(100)
	if err != nil || !changed {
		t.Fatalf("hot-path owner was not promoted: changed=%v err=%v", changed, err)
	}
	value, err := New(root).read()
	if err != nil {
		t.Fatal(err)
	}
	if state := value["owners"].(map[string]any)["queued"].(map[string]any)["state"]; state != "ACTIVE" {
		t.Fatalf("hot-path owner remained blocked by active overlap: %v", state)
	}
}

func TestPromoteUsesOwnerOrderFromAnySlice(t *testing.T) {
	store := New(t.TempDir())
	value := map[string]any{
		"owners": map[string]any{
			"a-newer": map[string]any{"state": "QUEUED", "scope": []any{"scope"}, "task_id": "a", "job_id": "a-job", "queued_at": json.Number("100")},
			"z-older": map[string]any{"state": "QUEUED", "scope": []any{"scope"}, "task_id": "z", "job_id": "z-job", "queued_at": json.Number("100")},
		},
		ownerOrderKey: []any{"z-older", "a-newer"},
	}
	if !store.promote(value, 101) {
		t.Fatal("queued owner was not promoted")
	}
	owners := value["owners"].(map[string]any)
	if owners["z-older"].(map[string]any)["state"] != "ACTIVE" || owners["a-newer"].(map[string]any)["state"] != "QUEUED" {
		t.Fatalf("owner order from []any was ignored: %#v", owners)
	}
}

func TestReconcileExpiresBeforePromotingQueuedOwner(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	fixture := `{"version":1,"owners":{"expired":{"acquired_at":1,"deadline_at":99,"scope":["src"],"state":"ACTIVE","task_id":"expired"},"queued":{"job_id":"job","queued_at":99,"scope":["src/sub"],"state":"QUEUED","task_id":"queued"}}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := New(root).Reconcile(100)
	if err != nil || !changed {
		t.Fatalf("expired owner did not trigger reconciliation: changed=%v err=%v", changed, err)
	}
	value, err := New(root).read()
	if err != nil {
		t.Fatal(err)
	}
	owners := value["owners"].(map[string]any)
	if owners["expired"].(map[string]any)["state"] != "RELEASED" {
		t.Fatal("synchronous owner was not expired")
	}
	if owner := owners["queued"].(map[string]any); owner["state"] != "ACTIVE" || owner["acquired_at"] != json.Number("100") {
		t.Fatalf("queued owner was not promoted after expiry: %#v", owner)
	}
}

func TestReconcileParityOutcome(t *testing.T) {
	root := os.Getenv("PURSER_LEASE_RECONCILE_ROOT")
	if root == "" {
		t.Skip("parity helper")
	}
	now, err := strconv.ParseFloat(os.Getenv("PURSER_LEASE_RECONCILE_NOW"), 64)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := New(root).Reconcile(now)
	if err != nil {
		t.Fatal(err)
	}
	value, err := New(root).read()
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{"changed": changed, "owners": value["owners"]})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("LEASE_RECONCILE_RESULT:" + string(result))
}

func TestRetentionParityReplay(t *testing.T) {
	root, fixturePath := os.Getenv("PURSER_LEASE_RETENTION_ROOT"), os.Getenv("PURSER_LEASE_RETENTION_FIXTURE")
	if root == "" || fixturePath == "" {
		t.Skip("parity helper")
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Case struct {
			Now        float64 `json:"now"`
			Operations []struct {
				Method string   `json:"method"`
				Owner  string   `json:"owner"`
				TaskID string   `json:"task_id"`
				Value  float64  `json:"value"`
				Paths  []string `json:"paths"`
			} `json:"operations"`
		} `json:"case"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	store := New(root)
	snapshots := make([]map[string]any, 0, len(fixture.Case.Operations))
	for _, op := range fixture.Case.Operations {
		var result any
		switch op.Method {
		case "set_retention_after":
			result = store.SetRetentionAfter(op.Owner, op.Value, fixture.Case.Now)
		case "owner_state":
			result, err = store.OwnerState(op.Owner, fixture.Case.Now)
		case "overlapping_owners":
			result, err = store.OverlappingOwners(op.Owner, fixture.Case.Now)
			if err != nil {
				result = map[string]any{"error": err.Error()}
				err = nil
			}
		case "record_conflicts":
			err = store.RecordConflicts(op.Paths, fixture.Case.Now)
		case "extend_retention":
			result = store.ExtendRetention(op.Owner, op.Value, fixture.Case.Now)
		case "active":
			result, err = store.Active(fixture.Case.Now)
		case "release_task":
			result, err = store.ReleaseTask(op.TaskID, fixture.Case.Now)
		default:
			t.Fatalf("unknown method %q", op.Method)
		}
		if err != nil {
			t.Fatal(err)
		}
		switch value := result.(type) {
		case Result:
			result = map[string]any{"status": value.Status, "code": value.Code}
		}
		state, err := store.read()
		if err != nil {
			t.Fatal(err)
		}
		delete(state, ownerOrderKey)
		snapshots = append(snapshots, map[string]any{"result": result, "state": state})
	}
	encoded, err := json.Marshal(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("LEASE_RETENTION_RESULT:" + string(encoded))
}

func TestRetentionMethodsRejectMalformedRecordsWithoutPanicking(t *testing.T) {
	for _, tc := range []struct {
		name, record string
		check        func(*Store) bool
	}{
		{"owner-state", `{"version":1,"owners":{"x":{"state":"BROKEN","scope":["a"],"task_id":"t"}}}`, func(s *Store) bool {
			_, err := s.OwnerState("x", 1)
			return err != nil
		}},
		{"retention-owner", `{"version":1,"owners":{"x":{"state":"BROKEN","scope":["a"],"task_id":"t"}}}`, func(s *Store) bool {
			got := s.SetRetentionAfter("x", 2, 1)
			return got == (Result{"UNKNOWN", "LEASE_RETENTION_UNCERTAIN"})
		}},
		{"conflict-path", `{"version":1,"owners":{},"conflicts":[{"path":1,"recorded_at":1}]}`, func(s *Store) bool {
			return s.RecordConflicts([]string{"a"}, 1) != nil
		}},
		{"conflict-item", `{"version":1,"owners":{},"conflicts":[1]}`, func(s *Store) bool {
			return s.RecordConflicts([]string{"a"}, 1) != nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "scope_leases.json"), []byte(tc.record), 0600); err != nil {
				t.Fatal(err)
			}
			if !tc.check(New(root)) {
				t.Fatal("malformed record did not return its error/UNKNOWN outcome")
			}
		})
	}
}

func TestWritePrunesExpiredPythonLeaseEntries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "scope_leases.json")
	now := float64(time.Now().UnixNano()) / 1e9
	staleAt := now - 7*24*60*60 - 12*60*60
	fixture, err := json.Marshal(map[string]any{
		"version": 1,
		"owners": map[string]any{
			"release": map[string]any{"state": "ACTIVE", "scope": []string{"src"}, "task_id": "release", "acquired_at": now - 60},
			"expired": map[string]any{"state": "RELEASED", "scope": []string{"old"}, "task_id": "expired", "released_at": staleAt},
		},
		"conflicts": []any{
			map[string]any{"path": "stale", "recorded_at": staleAt},
			map[string]any{"path": "recent", "recorded_at": now - conflictRetentionSeconds + 60},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	if got := New(root).Release("release", now); got.Code != "LEASE_RELEASED" {
		t.Fatalf("state-changing release: %#v", got)
	}
	value, err := New(root).read()
	if err != nil {
		t.Fatal(err)
	}
	owners := value["owners"].(map[string]any)
	if _, exists := owners["expired"]; exists {
		t.Fatal("expired released owner survived next state-changing write")
	}
	conflicts := value["conflicts"].([]any)
	if len(conflicts) != 1 || conflicts[0].(map[string]any)["path"] != "recent" {
		t.Fatalf("Python conflict retention mismatch: %#v", conflicts)
	}
}
