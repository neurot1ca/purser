package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	case <-time.After(5 * time.Second):
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
	wantFiles := caseByName["sample_recorded_ranking_updated_launch_spec"]
	wantFiles.Leader = nil // Leader updates now require a completed strict race win.
	assertMechanicalFiles(t, root, wantFiles)

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
		PostMechanicalLearning(map[string]any{}, broken, t.TempDir(), "project", "session-1", mechanicalPending(), mechanicalBundle())
	})
	t.Run("launch failure is swallowed", func(t *testing.T) {
		state := t.TempDir()
		writeMechanicalInventory(t, state)
		startMechanicalShadowDetached = func([]string, []string, string) error { return errors.New("launch failed") }
		t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
		PostMechanicalLearning(map[string]any{}, state, filepath.Join(t.TempDir(), "missing"), "project", "session-1", mechanicalPending(), mechanicalBundle())
		if _, err := os.Stat(filepath.Join(state, "opencode", "leader.json")); !os.IsNotExist(err) {
			t.Fatalf("launch failure changed leader: %v", err)
		}
	})
	t.Run("feature disabled by config", func(t *testing.T) {
		state := t.TempDir()
		PostMechanicalLearning(map[string]any{"mechanical_opencode_enabled": false}, state, t.TempDir(), "project", "session-1", mechanicalPending(), mechanicalBundle())
		entries, err := os.ReadDir(state)
		if err != nil || len(entries) != 0 {
			t.Fatalf("disabled feature touched state: entries=%v err=%v", entries, err)
		}
	})
}

func TestStartMechanicalShadowLaunchFailureRemovesLock(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	openMechanicalRace(t, root)
	startMechanicalShadowDetached = func([]string, []string, string) error { return os.ErrNotExist }
	t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
	_, err := startMechanicalShadow(root, "project", "repo", "prompt", "report", "served", "thread-1")
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
	openMechanicalRace(t, root)
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
	PostMechanicalLearning(map[string]any{}, root, "unused-plugin-root", "project", "session-1", mechanicalPending(), rejected)
	if len(launched) != 0 {
		t.Fatalf("rejected result launched shadow: %#v", launched)
	}
	PostMechanicalLearning(map[string]any{}, root, "unused-plugin-root", "project", "session-1", mechanicalPending(), mechanicalBundle())
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
			openMechanicalRace(t, root)
			launches := 0
			startMechanicalShadowDetached = func([]string, []string, string) error { launches++; return nil }
			t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
			bundle := map[string]any{
				"validation": "UNKNOWN", "worker_validation": tc.worker,
				"reviewer1_result": tc.reviewer, "implementer_report": "accepted",
			}
			PostMechanicalLearning(map[string]any{}, root, "unused-plugin-root", "project", "session-1", mechanicalPending(), bundle)
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

func TestMechanicalScoreThresholdAndSmoothing(t *testing.T) {
	for _, tc := range []struct {
		samples  []bool
		score    float64
		measured bool
	}{
		{[]bool{true, true}, 0, false},
		{[]bool{true, true, true}, .8, true},
		{[]bool{true, true, true, true, true}, 6.0 / 7.0, true},
		{[]bool{true, true, true, true, true, true, true, true, true, false}, 10.0 / 12.0, true},
	} {
		got, measured := MechanicalScore(tc.samples)
		if got != tc.score || measured != tc.measured {
			t.Fatalf("MechanicalScore(%v) = %v,%v want %v,%v", tc.samples, got, measured, tc.score, tc.measured)
		}
	}
	score33, _ := MechanicalScore([]bool{true, true, true})
	score910, _ := MechanicalScore([]bool{true, true, true, true, true, true, true, true, true, false})
	score55, _ := MechanicalScore([]bool{true, true, true, true, true})
	if score33 >= score910 || score55 <= score910 {
		t.Fatalf("smoothed ordering 3/3=%v 9/10=%v 5/5=%v", score33, score910, score55)
	}
}

func TestReconcileMechanicalInventoryOpensOnlyPostBaselineRace(t *testing.T) {
	root := t.TempDir()
	if err := ReconcileMechanicalInventory(root, []string{"vendor/base-a", "vendor/base-b"}, "2026-10-16"); err != nil {
		t.Fatal(err)
	}
	active, _ := readMechanicalCandidates(t, root)
	if candidateByID(active, "vendor/base-a") == nil {
		t.Fatal("baseline candidate missing")
	}
	data, _ := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	var inventory map[string]any
	_ = json.Unmarshal(data, &inventory)
	if _, exists := inventory["race"]; exists {
		t.Fatal("baseline population opened a race")
	}
	// Make a measured leader before the next catalog refresh.
	if err := recordMechanicalSample(root, "vendor/base-a", true); err != nil {
		t.Fatal(err)
	}
	if err := recordMechanicalSample(root, "vendor/base-a", true); err != nil {
		t.Fatal(err)
	}
	if err := recordMechanicalSample(root, "vendor/base-a", true); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileMechanicalInventory(root, []string{"vendor/base-a", "vendor/base-b", "vendor/new-a", "vendor/new-b"}, "2026-10-17"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	_ = json.Unmarshal(data, &inventory)
	race, ok := inventory["race"].(map[string]any)
	if !ok || race["stage"] != "newcomer_bracket" {
		t.Fatalf("race = %#v", inventory["race"])
	}
	if got := race["entrants"].([]any); !reflect.DeepEqual(got, []any{"vendor/new-a", "vendor/new-b"}) {
		t.Fatalf("bracket entrants = %#v", got)
	}
}

func TestReconcileLegacyInventoryStartsBracketOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{"catalog_date": "2026-10-15", "candidates": []any{
		map[string]any{"id": "vendor/leader", "status": "measured", "mechanical_samples": []any{true, true, true}},
		map[string]any{"id": "vendor/new-a", "status": "unmeasured", "mechanical_samples": []any{}},
		map[string]any{"id": "vendor/new-b", "status": "unmeasured", "mechanical_samples": []any{}},
	}, "retired_candidates": []any{}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	ids := []string{"vendor/leader", "vendor/new-a", "vendor/new-b"}
	if err := ReconcileMechanicalInventory(root, ids, "2026-10-16"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	var inventory map[string]any
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	race, _ := inventory["race"].(map[string]any)
	if inventory["catalog_baseline"] != "2026-10-16" || race == nil || race["stage"] != "newcomer_bracket" || !reflect.DeepEqual(race["entrants"], []any{"vendor/new-a", "vendor/new-b"}) {
		t.Fatalf("legacy migration baseline=%v race=%#v", inventory["catalog_baseline"], race)
	}
	if err := ReconcileMechanicalInventory(root, ids, "2026-10-17"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	_ = json.Unmarshal(data, &inventory)
	race, _ = inventory["race"].(map[string]any)
	if race == nil || !reflect.DeepEqual(race["entrants"], []any{"vendor/new-a", "vendor/new-b"}) {
		t.Fatalf("second reconcile replaced race: %#v", race)
	}
}

func TestMechanicalShadowSkipsPendingBracket(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	path := filepath.Join(root, "opencode", "candidates.json")
	data, _ := os.ReadFile(path)
	var inventory map[string]any
	_ = json.Unmarshal(data, &inventory)
	inventory["race"] = map[string]any{"started": true, "finished": false, "stage": "newcomer_bracket", "entrants": []any{"shadow", "runner-up", "next"}, "samples": map[string]any{}, "session_claims": []any{}}
	data, _ = json.Marshal(inventory)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	started, err := startMechanicalShadow(root, "project", "repo", "prompt", "report", "", "session-bracket")
	if err != nil || started {
		t.Fatalf("pending bracket shadow started=%v err=%v", started, err)
	}
}

func TestMechanicalRaceSessionClaimAndShadowSampling(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	path := filepath.Join(root, "opencode", "candidates.json")
	data, _ := os.ReadFile(path)
	var inventory map[string]any
	_ = json.Unmarshal(data, &inventory)
	inventory["race"] = map[string]any{"started": true, "finished": false, "stage": "newcomer_vs_leader", "leader": "runner-up", "entrants": []any{"shadow", "runner-up"}, "samples": map[string]any{}, "session_claims": []any{}, "notice_claim_ids": []any{}}
	data, _ = json.Marshal(inventory)
	_ = os.WriteFile(path, data, 0600)
	launches := 0
	startMechanicalShadowDetached = func([]string, []string, string) error { launches++; return nil }
	t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
	started, err := startMechanicalShadow(root, "project", "repo", "prompt", "report", "", "session-1")
	if err != nil || !started {
		t.Fatalf("first run = %v,%v", started, err)
	}
	started, err = startMechanicalShadow(root, "project", "repo", "prompt", "report", "", "session-1")
	if err != nil || started || launches != 1 {
		t.Fatalf("duplicate run = %v,%v launches=%d", started, err, launches)
	}
	_ = os.Remove(filepath.Join(root, "projects", "project", "opencode_shadows", "running.lock"))
	if err = recordMechanicalRaceSample(root, "shadow", true); err != nil {
		t.Fatal(err)
	}
	started, err = startMechanicalShadow(root, "project", "repo", "prompt", "report", "", "session-2")
	if err != nil || !started || launches != 2 {
		t.Fatalf("second shadow run = %v,%v launches=%d", started, err, launches)
	}
	jobs, err := filepath.Glob(filepath.Join(root, "projects", "project", "opencode_shadows", "*.json"))
	if err != nil || len(jobs) != 2 {
		t.Fatalf("shadow jobs = %v, err=%v", jobs, err)
	}
	models := make([]string, 0, len(jobs))
	for _, jobPath := range jobs {
		jobData, readErr := os.ReadFile(jobPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var job map[string]any
		if readErr = json.Unmarshal(jobData, &job); readErr != nil {
			t.Fatal(readErr)
		}
		models = append(models, stringValue(job["model"]))
	}
	if len(models) != 2 || models[0] != "shadow" || models[1] != "shadow" {
		t.Fatalf("shadow model = %v", models)
	}
	for i := 1; i < mechanicalMaxSamples; i++ {
		if err := recordMechanicalRaceSample(root, "shadow", true); err != nil {
			t.Fatal(err)
		}
	}
	data, _ = os.ReadFile(path)
	_ = json.Unmarshal(data, &inventory)
	race, _ := inventory["race"].(map[string]any)
	if race["finished"] != true || race["winner"] != "shadow" {
		t.Fatalf("shadow race did not finish: %#v", race)
	}
}

func TestStartMechanicalShadowNewcomerRaceAlwaysPicksChallenger(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	path := filepath.Join(root, "opencode", "candidates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	inventory["race"] = map[string]any{
		"started": true, "finished": false, "stage": "newcomer_vs_leader",
		"entrants":       []any{"shadow", "runner-up"},
		"samples":        map[string]any{"shadow": []any{true}, "runner-up": []any{}},
		"session_claims": []any{}, "notice_claim_ids": []any{},
	}
	data, err = json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var launchedModel string
	startMechanicalShadowDetached = func(argv, _ []string, _ string) error {
		jobData, readErr := os.ReadFile(argv[2])
		if readErr != nil {
			return readErr
		}
		var job map[string]any
		if readErr = json.Unmarshal(jobData, &job); readErr != nil {
			return readErr
		}
		launchedModel = stringValue(job["model"])
		return nil
	}
	t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
	started, err := startMechanicalShadow(root, "project", "repo", "prompt", "report", "", "session-1")
	if err != nil || !started {
		t.Fatalf("start = %v, %v", started, err)
	}
	if launchedModel != "shadow" {
		t.Fatalf("launched model = %q, want challenger shadow", launchedModel)
	}
}

func TestMechanicalNewcomerLossKeepsLeader(t *testing.T) {
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	openMechanicalRace(t, root)
	path := filepath.Join(root, "opencode", "candidates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var initial map[string]any
	if err = json.Unmarshal(data, &initial); err != nil {
		t.Fatal(err)
	}
	for _, raw := range initial["candidates"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["id"] == "runner-up" {
			candidate["mechanical_samples"] = []any{false, false, false}
		}
	}
	data, _ = json.Marshal(initial)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"shadow", "runner-up"} {
		for i := 0; i < mechanicalMaxSamples; i++ {
			if err := recordMechanicalRaceSample(root, model, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err = os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	race := inventory["race"].(map[string]any)
	if race["finished"] != true || race["winner"] != "runner-up" {
		t.Fatalf("loss state = %#v", race)
	}
	if _, err = os.Stat(filepath.Join(root, "opencode", "leader.json")); !os.IsNotExist(err) {
		t.Fatalf("tie changed leader: %v", err)
	}
}

func TestMechanicalNewcomerUsesLeaderHistoryForStrictWin(t *testing.T) {
	for _, tc := range []struct {
		name            string
		leaderSuccesses int
		leaderSamples   int
		challenger      []bool
		wantFinished    bool
		wantWinner      string
	}{
		{"three of three stays open against nine of ten", 9, 10, []bool{true, true, true}, false, ""},
		{"five of five beats nine of ten", 9, 10, []bool{true, true, true, true, true}, true, "challenger"},
		{"ten samples without a win loses", 5, 10, []bool{true, false, false, true, true, false, false, false, false, false}, true, "leader"},
		{"impossible to catch up loses early", 20, 20, []bool{true, true, true}, true, "leader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "opencode", "candidates.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			leaderHistory := make([]any, 0, tc.leaderSamples)
			for i := 0; i < tc.leaderSuccesses; i++ {
				leaderHistory = append(leaderHistory, true)
			}
			for i := tc.leaderSuccesses; i < tc.leaderSamples; i++ {
				leaderHistory = append(leaderHistory, false)
			}
			inventory := map[string]any{"candidates": []any{
				map[string]any{"id": "leader", "status": "measured", "mechanical_samples": leaderHistory},
				map[string]any{"id": "challenger", "status": "unmeasured", "mechanical_samples": []any{}},
			}, "race": map[string]any{"started": true, "finished": false, "stage": "newcomer_vs_leader", "entrants": []any{"challenger", "leader"}, "samples": map[string]any{}, "session_claims": []any{}, "notice_claim_ids": []any{}}}
			data, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			rawSamples := make([]any, 0, len(tc.challenger))
			for _, success := range tc.challenger {
				rawSamples = append(rawSamples, success)
			}
			race := inventory["race"].(map[string]any)
			race["samples"] = map[string]any{"challenger": rawSamples}
			advanceMechanicalRace(root, inventory, race)
			if got := race["finished"] == true; got != tc.wantFinished {
				t.Fatalf("finished = %v, want %v; race=%#v", got, tc.wantFinished, race)
			}
			if got := stringValue(race["winner"]); got != tc.wantWinner {
				t.Fatalf("winner = %q, want %q; race=%#v", got, tc.wantWinner, race)
			}
		})
	}
}

func TestMechanicalCandidateSamplesKeepLeaderSnapshotConsistent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	inventory := map[string]any{"candidates": []any{
		map[string]any{"id": "leader", "status": "measured", "mechanical_samples": []any{true, true, true, false, false}},
	}, "race": map[string]any{"started": true, "finished": true, "winner": "leader", "entrants": []any{"leader", "old-leader"}}}
	data, _ := json.Marshal(inventory)
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "opencode", "leader.json"), []byte(`{"model":"leader","successes":3,"sample_count":5,"announced":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := recordMechanicalSample(root, "leader", true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "opencode", "leader.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err = json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["sample_count"] != float64(6) || snapshot["successes"] != float64(4) {
		t.Fatalf("leader snapshot=%v, want candidate history 4/6", snapshot)
	}
}

func TestMechanicalBracketTieKeepsStableOrder(t *testing.T) {
	inventory := map[string]any{"candidates": []any{map[string]any{"id": "leader", "status": "measured", "mechanical_samples": []any{true, true, true}}}}
	samples := map[string]any{}
	for _, model := range []string{"left", "right"} {
		values := make([]any, mechanicalMaxSamples)
		for i := range values {
			values[i] = false
		}
		samples[model] = values
	}
	race := map[string]any{"stage": "newcomer_bracket", "entrants": []any{"left", "right", "next"}, "samples": samples}
	advanceMechanicalRace(t.TempDir(), inventory, race)
	if got := race["entrants"].([]any); !reflect.DeepEqual(got, []any{"left", "next"}) {
		t.Fatalf("tie entrants = %#v", got)
	}
}

func TestMechanicalVerificationPrompt(t *testing.T) {
	prompt := mechanicalVerificationPrompt(map[string]any{"changed_paths": []any{"a.go", "b.go"}, "implementer_report": "report"})
	for _, want := range []string{"Read-only", "a.go, b.go", "report", "Do not modify"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt %q lacks %q", prompt, want)
		}
	}
}

func TestPostMechanicalNonMechanicalRaceUsesReadOnlyPromptAndNoRaceDoesNothing(t *testing.T) {
	t.Cleanup(func() { startMechanicalShadowDetached = proc.StartDetached })
	for _, raceOpen := range []bool{false, true} {
		root := t.TempDir()
		writeMechanicalInventory(t, root)
		if raceOpen {
			openMechanicalRace(t, root)
		}
		var job map[string]any
		startMechanicalShadowDetached = func(argv, _ []string, _ string) error {
			data, err := os.ReadFile(argv[2])
			if err != nil {
				return err
			}
			return json.Unmarshal(data, &job)
		}
		pending := map[string]any{"agent": "codex", "thread_id": "thread-non-mechanical", "repo_root": "repo"}
		bundle := map[string]any{"validation": "PASS", "worker_validation": mechanicalWorkerValidation("PASS", 0), "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "implementer_report": "accepted report", "changed_paths": []any{"change.go"}}
		PostMechanicalLearning(map[string]any{}, root, "unused", "project", "session-non-mechanical", pending, bundle)
		if raceOpen {
			if job == nil || !strings.Contains(stringValue(job["prompt"]), "Read-only") || !strings.Contains(stringValue(job["prompt"]), "change.go") {
				t.Fatalf("non-mechanical shadow job = %#v", job)
			}
		} else if job != nil {
			t.Fatalf("no-race run created shadow job: %#v", job)
		}
	}
}

func openMechanicalRace(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "opencode", "candidates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	inventory["race"] = map[string]any{"started": true, "finished": false, "stage": "newcomer_vs_leader", "entrants": []any{"shadow", "runner-up"}, "samples": map[string]any{}, "session_claims": []any{}, "notice_claim_ids": []any{}}
	data, err = json.Marshal(inventory)
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
	if want.Leader != nil {
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
}

func mechanicalPending() map[string]any {
	return map[string]any{"mechanical_opencode": true, "agent": "opencode", "model": "served", "shadow_prompt": "prompt", "repo_root": "repo", "thread_id": "thread-1"}
}
func mechanicalBundle() map[string]any {
	return map[string]any{"validation": "PASS", "worker_validation": mechanicalWorkerValidation("PASS", 0), "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "implementer_report": "accepted"}
}
