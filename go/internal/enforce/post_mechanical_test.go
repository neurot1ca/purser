package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

	python, script, job := "__PYTHON__", filepath.Join("__ORCH__", "mechanical_learning.py"), "__JOB__"
	spec := shadowSpec(python, script, job)
	want := caseByName["sample_recorded_ranking_updated_launch_spec"].Launch
	gotArgv := make([]any, len(spec.Argv))
	for i, arg := range spec.Argv {
		gotArgv[i] = strings.ReplaceAll(arg, filepath.Join("__ORCH__", "mechanical_learning.py"), "__SCRIPT__")
	}
	got := map[string]any{"argv": gotArgv, "env": nil, "cwd": strings.ReplaceAll(spec.Cwd, ".", "__ORCH__")}
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
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	writeMechanicalInventory(t, root)
	err := startMechanicalShadow(root, t.TempDir(), "project", "repo", "prompt", "report", "served")
	if err == nil {
		t.Fatal("startMechanicalShadow() error = nil, want missing Python error")
	}
	lock := filepath.Join(root, "projects", "project", "opencode_shadows", "running.lock")
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("running.lock stat error = %v, want file removed after launch failure", err)
	}
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
	return map[string]any{"validation": "PASS", "reviewer1_result": "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", "implementer_report": "accepted"}
}
