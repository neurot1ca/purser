package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/proc"
)

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
