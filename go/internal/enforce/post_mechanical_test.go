package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"purser-hook/internal/proc"
)

func TestReconcileMechanicalInventoryTransitions(t *testing.T) {
	root := t.TempDir()
	writeReconciliationInventory(t, root)
	if err := recordMechanicalSample(root, "vendor/served", true); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileMechanicalInventory(root, []string{"vendor/new-a", "vendor/served", "vendor/shadow", "vendor/new-b"}, "2026-10-16"); err != nil {
		t.Fatal(err)
	}
	active, history := readMechanicalCandidates(t, root)
	if got := candidateIDs(active); !reflect.DeepEqual(got, []string{"vendor/new-a", "vendor/served", "vendor/shadow", "vendor/new-b"}) {
		t.Fatalf("active IDs = %v", got)
	}
	for _, id := range []string{"vendor/new-a", "vendor/new-b"} {
		candidate := candidateByID(active, id)
		if candidate["status"] != "unmeasured" || len(candidate["mechanical_samples"].([]any)) != 0 {
			t.Fatalf("new candidate %q = %#v", id, candidate)
		}
	}
	if got := candidateByID(active, "vendor/served")["mechanical_samples"].([]any); len(got) != 5 || got[4] != true {
		t.Fatalf("surviving samples = %#v", got)
	}
	if err := ReconcileMechanicalInventory(root, []string{"vendor/served", "vendor/new-a"}, "2026-10-17"); err != nil {
		t.Fatal(err)
	}
	active, history = readMechanicalCandidates(t, root)
	if candidateByID(active, "vendor/shadow") != nil || candidateByID(active, "vendor/new-b") != nil {
		t.Fatalf("retired model remained active: %v", candidateIDs(active))
	}
	if selected := firstOpenCodeCandidate(active); selected == "vendor/shadow" || selected == "vendor/new-b" {
		t.Fatalf("retired model selected: %q", selected)
	}
	if candidateByID(history, "vendor/shadow") == nil || candidateByID(history, "vendor/new-b") == nil {
		t.Fatalf("retired history missing models: %v", candidateIDs(history))
	}
	if err := recordMechanicalSample(root, "vendor/shadow", false); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileMechanicalInventory(root, []string{"vendor/shadow", "vendor/served", "vendor/new-a"}, "2026-10-18"); err != nil {
		t.Fatal(err)
	}
	active, history = readMechanicalCandidates(t, root)
	if got := candidateByID(active, "vendor/shadow")["mechanical_samples"].([]any); len(got) != 1 || got[0] != false {
		t.Fatalf("reappeared late sample = %#v", got)
	}
	if candidateByID(history, "vendor/shadow") != nil {
		t.Fatalf("reappeared model remains retired: %v", candidateIDs(history))
	}
}

func TestReconcileMechanicalInventoryCreatesMissingFile(t *testing.T) {
	root := t.TempDir()
	if err := ReconcileMechanicalInventory(root, []string{"vendor/first", "vendor/second"}, "2026-10-16"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory["catalog_date"] != "2026-10-16" || inventory["catalog_baseline"] != "2026-10-16" {
		t.Fatalf("catalog dates = %#v", inventory)
	}
	if history, ok := inventory["retired_candidates"].([]any); !ok || len(history) != 0 {
		t.Fatalf("retired candidates = %#v", inventory["retired_candidates"])
	}
	active, ok := inventory["candidates"].([]any)
	if !ok || len(active) != 2 {
		t.Fatalf("active candidates = %#v", inventory["candidates"])
	}
	for _, id := range []string{"vendor/first", "vendor/second"} {
		candidate := candidateByID(active, id)
		if candidate == nil || candidate["status"] != "unmeasured" {
			t.Fatalf("new candidate %q = %#v", id, candidate)
		}
		if samples, ok := candidate["mechanical_samples"].([]any); !ok || len(samples) != 0 {
			t.Fatalf("new candidate samples %q = %#v", id, candidate["mechanical_samples"])
		}
	}
}

func TestReconcileMechanicalInventoryMalformedInputPreservesFile(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	path := filepath.Join(root, "opencode", "candidates.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ids  []string
		date string
	}{{[]string{"bad/id/extra"}, "2026-10-16"}, {[]string{"vendor/model"}, "bad-date"}, {nil, "2026-10-16"}} {
		if err := ReconcileMechanicalInventory(root, tc.ids, tc.date); err == nil {
			t.Fatalf("ReconcileMechanicalInventory(%v, %q) succeeded", tc.ids, tc.date)
		}
	}
	if err := os.WriteFile(path, []byte(`{"candidates":[{"id":3}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	malformed, _ := os.ReadFile(path)
	if err := ReconcileMechanicalInventory(root, []string{"vendor/model"}, "2026-10-16"); err == nil {
		t.Fatal("malformed inventory was accepted")
	}
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(after, malformed) || reflect.DeepEqual(after, before) {
		t.Fatal("malformed inventory was not preserved byte-for-byte")
	}
}

func TestMechanicalRefreshAndSampleShareInventoryLock(t *testing.T) {
	root := t.TempDir()
	writeReconciliationInventory(t, root)
	_, unlock, err := lockCandidateInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results <- ReconcileMechanicalInventory(root, []string{"vendor/served", "vendor/new-model"}, "2026-10-16")
	}()
	go func() { defer wg.Done(); results <- recordMechanicalSample(root, "vendor/served", false) }()
	time.Sleep(20 * time.Millisecond)
	unlock()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh/sample operations did not finish after lock release")
	}
	close(results)
	for result := range results {
		if result != nil {
			t.Fatal(result)
		}
	}
	active, _ := readMechanicalCandidates(t, root)
	if candidateByID(active, "vendor/new-model") == nil {
		t.Fatal("refresh update was lost")
	}
	samples := candidateByID(active, "vendor/served")["mechanical_samples"].([]any)
	if len(samples) != 5 || samples[4] != false {
		t.Fatalf("sample update was lost: %#v", samples)
	}
}

func writeReconciliationInventory(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "opencode", "candidates.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"candidates": []any{
		map[string]any{"id": "vendor/served", "status": "available", "mechanical_samples": []any{true, false, true, true}},
		map[string]any{"id": "vendor/runner-up", "status": "measured", "mechanical_samples": []any{true, true, true, false, false}},
		map[string]any{"id": "vendor/shadow", "status": "available", "mechanical_samples": []any{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readMechanicalCandidates(t *testing.T, root string) ([]any, []any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	active, _ := inventory["candidates"].([]any)
	history, _ := inventory["retired_candidates"].([]any)
	return active, history
}

func candidateByID(items []any, id string) map[string]any {
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok && item["id"] == id {
			return item
		}
	}
	return nil
}

func candidateIDs(items []any) []string {
	ids := make([]string, 0, len(items))
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			ids = append(ids, item["id"].(string))
		}
	}
	return ids
}

func TestPostMechanicalPythonFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/post_mechanical.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name   string `json:"name"`
			Result struct {
				Inventory map[string]any `json:"inventory"`
				Leader    map[string]any `json:"leader"`
				Launch    map[string]any `json:"launch"`
				Job       map[string]any `json:"job"`
			} `json:"result"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "mechanical_learning.record_sample+start_shadow" || len(fixture.Cases) != 4 {
		t.Fatalf("unexpected fixture oracle/case count: %q %d", fixture.Oracle, len(fixture.Cases))
	}
	caseByName := map[string]struct {
		Inventory map[string]any `json:"inventory"`
		Leader    map[string]any `json:"leader"`
		Launch    map[string]any `json:"launch"`
		Job       map[string]any `json:"job"`
	}{}
	for _, tc := range fixture.Cases {
		caseByName[tc.Name] = tc.Result
	}
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	if err := recordMechanicalSample(root, "served", true); err != nil {
		t.Fatal(err)
	}
	assertMechanicalFiles(t, root, caseByName["sample_recorded_ranking_updated_launch_spec"])

	executable, job := "__EXECUTABLE__", filepath.Join("opencode_shadows", "__JOB__")
	spec := shadowSpec(executable, job)
	want := caseByName["sample_recorded_ranking_updated_launch_spec"].Launch
	gotArgv := make([]any, len(spec.Argv))
	for i, arg := range spec.Argv {
		if arg == job {
			arg = "__JOB__"
		}
		gotArgv[i] = arg
	}
	got := map[string]any{"argv": gotArgv, "env": nil, "cwd": filepath.Base(spec.Cwd)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("launch spec = %#v, want %#v", got, want)
	}

	t.Run("sample write failure is swallowed", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.MkdirAll(filepath.Join(broken, "opencode", "candidates.json"), 0700); err != nil {
			t.Fatal(err)
		}
		PostMechanicalLearning(map[string]any{}, broken, t.TempDir(), "project", mechanicalPending(), mechanicalBundle())
	})
	t.Run("launch failure is swallowed", func(t *testing.T) {
		state := t.TempDir()
		writeMechanicalInventory(t, state)
		startMechanicalShadowDetached = func([]string, []string, string) error { return errors.New("launch failed") }
		t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
		PostMechanicalLearning(map[string]any{}, state, filepath.Join(t.TempDir(), "missing"), "project", mechanicalPending(), mechanicalBundle())
		if _, err := os.Stat(filepath.Join(state, "opencode", "leader.json")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("feature disabled by config", func(t *testing.T) {
		state := t.TempDir()
		PostMechanicalLearning(map[string]any{"mechanical_opencode_enabled": false}, state, t.TempDir(), "project", mechanicalPending(), mechanicalBundle())
		entries, err := os.ReadDir(state)
		if err != nil || len(entries) != 0 {
			t.Fatalf("disabled feature touched state: entries=%v err=%v", entries, err)
		}
	})
}

func TestStartMechanicalShadowLaunchFailureRemovesLock(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	startMechanicalShadowDetached = func([]string, []string, string) error { return os.ErrNotExist }
	t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
	_, err := startMechanicalShadow(root, "project", "repo", "prompt", "report", "served")
	if err == nil {
		t.Fatal("startMechanicalShadow() error = nil, want launch error")
	}
	lock := filepath.Join(root, "projects", "project", "opencode_shadows", "running.lock")
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("running.lock stat error = %v, want file removed after launch failure", err)
	}
}

func TestStartMechanicalShadowLaunchesBundledCommand(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	writeMechanicalCadence(t, root, "project", 69, 0)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var launched []string
	var launchedDir string
	startMechanicalShadowDetached = func(argv, _ []string, dir string) error {
		launched, launchedDir = argv, dir
		return nil
	}
	t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
	rejected := mechanicalBundle()
	rejected["validation"] = "FAIL"
	PostMechanicalLearning(map[string]any{}, root, "unused-plugin-root", "project", mechanicalPending(), rejected)
	if len(launched) != 0 {
		t.Fatalf("rejected result launched shadow: %#v", launched)
	}
	PostMechanicalLearning(map[string]any{}, root, "unused-plugin-root", "project", mechanicalPending(), mechanicalBundle())
	if len(launched) != 3 || launched[0] != executable || launched[1] != "mechanical-shadow" {
		t.Fatalf("launch argv = %#v, want current executable mechanical-shadow <job>", launched)
	}
	if launchedDir != filepath.Dir(launched[2]) {
		t.Fatalf("launch cwd = %q, want job directory %q", launchedDir, filepath.Dir(launched[2]))
	}
	var job map[string]any
	data, err := os.ReadFile(launched[2])
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"model": "shadow", "prompt": "prompt", "repository": "repo", "accepted_report": "accepted", "project_key": "project", "state_root": root} {
		if job[key] != want {
			t.Errorf("job[%q] = %#v, want %q", key, job[key], want)
		}
	}
	if scratch, ok := job["scratch"].(string); !ok || filepath.Dir(scratch) != launchedDir {
		t.Errorf("job scratch = %#v, want path under %q", job["scratch"], launchedDir)
	}
}

func TestPostMechanicalAcceptanceUsesWorkerValidationAndReviewer(t *testing.T) {
	tests := []struct {
		name     string
		worker   map[string]any
		reviewer string
		want     bool
	}{
		{name: "unknown bundle validation with worker pass and clean review", worker: mechanicalWorkerValidation("PASS", 0), reviewer: "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", want: true},
		{name: "worker validation fail", worker: mechanicalWorkerValidation("FAIL", 1), reviewer: "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"},
		{name: "reviewer finding", worker: mechanicalWorkerValidation("PASS", 0), reviewer: "CHANGES_REQUIRED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeMechanicalInventory(t, root)
			writeMechanicalCadence(t, root, "project", 69, 0)
			launches := 0
			startMechanicalShadowDetached = func([]string, []string, string) error { launches++; return nil }
			t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
			bundle := map[string]any{
				"validation": "UNKNOWN", "worker_validation": tc.worker,
				"reviewer1_result": tc.reviewer, "implementer_report": "accepted",
			}
			PostMechanicalLearning(map[string]any{}, root, "unused-plugin-root", "project", mechanicalPending(), bundle)
			if got := launches > 0; got != tc.want {
				t.Fatalf("shadow launch = %v, want accepted=%v (launches=%d)", got, tc.want, launches)
			}
			var inventory map[string]any
			data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, &inventory); err != nil {
				t.Fatal(err)
			}
			candidates := inventory["candidates"].([]any)
			for _, value := range candidates {
				candidate := value.(map[string]any)
				if candidate["id"] == "served" {
					samples := candidate["mechanical_samples"].([]any)
					if got := samples[len(samples)-1] == true; got != tc.want {
						t.Fatalf("latest mechanical sample success = %v, want %v", got, tc.want)
					}
					return
				}
			}
			t.Fatal("served candidate missing")
		})
	}
}

func TestMechanicalShadowCadence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		turnCount int
		want      bool
	}{
		{name: "69 turns after last shadow", turnCount: 69, want: false},
		{name: "70 turns after last shadow", turnCount: 70, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeMechanicalCadence(t, root, "project", tc.turnCount, 1)
			_, eligible, unlock, err := advanceMechanicalShadowTurn(root, "project")
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if eligible != tc.want {
				t.Fatalf("eligible = %v, want %v", eligible, tc.want)
			}
		})
	}
}

func writeMechanicalCadence(t *testing.T, root, project string, turns, lastShadow int) {
	t.Helper()
	path := filepath.Join(root, "projects", project, "telemetry", "mechanical_shadow_cadence.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]int{"turn_count": turns, "last_shadow_turn": lastShadow})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func mechanicalWorkerValidation(status string, exitCode int) map[string]any {
	return map[string]any{"source": "WORKER_REPORTED", "status": status, "commands": []any{"go test ./..."}, "exit_codes": []any{exitCode}}
}

func writeMechanicalInventory(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "opencode", "candidates.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	value := map[string]any{"candidates": []any{
		map[string]any{"id": "served", "status": "available", "mechanical_samples": []any{true, false, true, true}},
		map[string]any{"id": "runner-up", "status": "measured", "mechanical_samples": []any{true, true, true, false, false}},
		map[string]any{"id": "shadow", "status": "available", "mechanical_samples": []any{}},
	}}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertMechanicalFiles(t *testing.T, root string, want struct {
	Inventory map[string]any `json:"inventory"`
	Leader    map[string]any `json:"leader"`
	Launch    map[string]any `json:"launch"`
	Job       map[string]any `json:"job"`
}) {
	t.Helper()
	var inventory map[string]any
	data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inventory, want.Inventory) {
		t.Fatalf("inventory = %#v, want %#v", inventory, want.Inventory)
	}
	var leader map[string]any
	data, err = os.ReadFile(filepath.Join(root, "opencode", "leader.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &leader); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leader, want.Leader) {
		t.Fatalf("leader = %#v, want %#v", leader, want.Leader)
	}
}

func mechanicalPending() map[string]any {
	return map[string]any{"mechanical_opencode": true, "agent": "opencode", "model": "served", "shadow_prompt": "prompt", "repo_root": "repo"}
}
func mechanicalBundle() map[string]any {
	return map[string]any{"validation": "PASS", "worker_validation": mechanicalWorkerValidation("PASS", 0), "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "implementer_report": "accepted"}
}
