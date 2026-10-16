package enforce

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootstrapMechanicalRaceRecordsAndResumes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	items := []any{}
	for _, id := range []string{"opencode/a-free", "opencode/b-free", "opencode/c-free", "opencode/d-free"} {
		items = append(items, map[string]any{"id": id, "status": "unmeasured", "mechanical_samples": []any{}})
	}
	data, _ := json.Marshal(map[string]any{"candidates": items, "retired_candidates": []any{}})
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	oldRunner := mechanicalBootstrapRunner
	mechanicalBootstrapRunner = func(_ context.Context, _ string, model, task string, _ time.Duration) bool {
		calls.Add(1)
		return model == "opencode/b-free" && task != "config_value"
	}
	t.Cleanup(func() { mechanicalBootstrapRunner = oldRunner })
	if err := BootstrapMechanicalRace(root); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 12 {
		t.Fatalf("runner calls=%d, want 12", got)
	}
	if err := BootstrapMechanicalRace(root); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 12 {
		t.Fatalf("finished race reran tasks: calls=%d", got)
	}
	raw, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Candidates []map[string]any         `json:"candidates"`
		Bootstrap  mechanicalBootstrapState `json:"bootstrap"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	if inventory.Bootstrap.Status != "finished" || inventory.Bootstrap.Winner != "opencode/b-free" || inventory.Bootstrap.StartedAt == "" || inventory.Bootstrap.FinishedAt == "" {
		t.Fatalf("bootstrap state=%+v", inventory.Bootstrap)
	}
	if inventory.Candidates[0]["id"] != "opencode/b-free" || inventory.Candidates[0]["status"] != "measured" {
		t.Fatalf("bootstrap candidate ranking=%v", inventory.Candidates)
	}
	for _, candidate := range inventory.Candidates {
		samples, _ := candidate["mechanical_samples"].([]any)
		if len(samples) != 3 {
			t.Fatalf("candidate %v samples=%v", candidate["id"], samples)
		}
	}
}

func TestBootstrapFixtureCheckerRequiresExactExpectedTree(t *testing.T) {
	expected, err := fs.Sub(mechanicalBootstrapFiles, "bootstrap_tasks/rename_symbol/expected")
	if err != nil {
		t.Fatal(err)
	}
	match := t.TempDir()
	if err := copyEmbeddedTree(expected, match); err != nil {
		t.Fatal(err)
	}
	if !equalEmbeddedTree(match, expected) {
		t.Fatal("matching output rejected")
	}
	entries, err := os.ReadDir(match)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(match, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.ReplaceAll(strings.TrimRight(string(data), "\r\n"), "\n", "\r\n"))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(match, ".opencode", "cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(match, ".opencode", "cache", "state.json"), []byte("generated"), 0600); err != nil {
		t.Fatal(err)
	}
	if !equalEmbeddedTree(match, expected) {
		t.Fatal("line ending, trailing newline, or hidden path rejected")
	}
	wrong := filepath.Join(match, "a.go")
	data, err := os.ReadFile(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrong, []byte(strings.Replace(string(data), "NewName", "OtherName", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if equalEmbeddedTree(match, expected) {
		t.Fatal("wrong edit accepted")
	}
}

func TestBootstrapMechanicalRaceRetiredEntrantStillFinishes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"candidates":[{"id":"opencode/a-free","status":"unmeasured","mechanical_samples":[]}],"retired_candidates":[]}`)
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareMechanicalBootstrap(root); err != nil {
		t.Fatal(err)
	}
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	raw, err := os.ReadFile(path)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		unlock()
		t.Fatal(err)
	}
	inventory["candidates"] = []any{}
	if err := writeBootstrapInventory(path, inventory); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	for task := range mechanicalBootstrapTasks {
		if err := recordMechanicalBootstrapResult(root, "opencode/a-free", task, true); err != nil {
			t.Fatalf("record retired task %d: %v", task, err)
		}
	}
	if err := finishMechanicalBootstrap(root); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var finished struct {
		Candidates []any                    `json:"candidates"`
		Bootstrap  mechanicalBootstrapState `json:"bootstrap"`
	}
	if err := json.Unmarshal(raw, &finished); err != nil {
		t.Fatal(err)
	}
	if finished.Bootstrap.Status != "finished" || finished.Bootstrap.Winner != "" {
		t.Fatalf("bootstrap=%+v", finished.Bootstrap)
	}
	for _, result := range finished.Bootstrap.Results["opencode/a-free"] {
		if result == nil || *result {
			t.Fatalf("retired result=%v, want false", result)
		}
	}
}

func TestBootstrapMechanicalRaceResumesWithoutRepeatingRecordedSample(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"candidates":[{"id":"opencode/a-free","status":"unmeasured","mechanical_samples":[]}],"retired_candidates":[]}`)
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareMechanicalBootstrap(root); err != nil {
		t.Fatal(err)
	}
	if err := recordMechanicalBootstrapResult(root, "opencode/a-free", 0, true); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	oldRunner := mechanicalBootstrapRunner
	mechanicalBootstrapRunner = func(_ context.Context, _ string, _, _ string, _ time.Duration) bool { calls.Add(1); return true }
	t.Cleanup(func() { mechanicalBootstrapRunner = oldRunner })
	if err := BootstrapMechanicalRace(root); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("reran recorded task or skipped unfinished tasks: calls=%d", calls.Load())
	}
	raw, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		Candidates []map[string]any `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	samples, _ := inventory.Candidates[0]["mechanical_samples"].([]any)
	if len(samples) != 3 {
		t.Fatalf("mechanical samples=%v, want 3 unique results", samples)
	}
}

func TestMechanicalNewcomerBracketRecordsSamplesAndAdvances(t *testing.T) {
	root := mechanicalBracketFixture(t)
	old := mechanicalBootstrapRunner
	mechanicalBootstrapRunner = func(_ context.Context, _, model, task string, _ time.Duration) bool {
		if model == "opencode/a-free" {
			return true
		}
		return task == "rename_symbol"
	}
	t.Cleanup(func() { mechanicalBootstrapRunner = old })
	if err := RunMechanicalNewcomerBracket(root); err != nil {
		t.Fatal(err)
	}
	inventory := readBracketInventory(t, root)
	race := inventory["race"].(map[string]any)
	if race["stage"] != "newcomer_vs_leader" || !reflect.DeepEqual(race["entrants"], []any{"opencode/a-free", "opencode/leader-free"}) {
		t.Fatalf("race=%#v", race)
	}
	if got := measuredLeader(inventory["candidates"].([]any)); got != "opencode/leader-free" {
		t.Fatalf("bracket changed measured leader: %q", got)
	}
	for _, candidate := range inventory["candidates"].([]any) {
		c := candidate.(map[string]any)
		if c["id"] == "opencode/a-free" {
			if c["status"] != "unmeasured" || len(c["mechanical_samples"].([]any)) != 0 {
				t.Fatalf("bracket changed routing candidate=%v", c)
			}
		}
	}
	results := race["bootstrap_results"].(map[string]any)
	if got := results["opencode/a-free"].([]any); len(got) != 3 || got[0] != true || got[1] != true || got[2] != true {
		t.Fatalf("bracket results=%v", got)
	}
}

func TestMechanicalLeaderRaceUsesBuiltInTasks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	inventory := map[string]any{"candidates": []any{
		map[string]any{"id": "leader", "status": "measured", "mechanical_samples": []any{true, false, false, false, false, false, false, false, false, false, false, false, false, false, false, false, false, false}},
		map[string]any{"id": "challenger", "status": "unmeasured", "mechanical_samples": []any{}},
	}, "race": map[string]any{"started": true, "finished": false, "stage": "newcomer_vs_leader", "leader": "leader", "entrants": []any{"challenger", "leader"}, "samples": map[string]any{}, "session_claims": []any{}}}
	data, _ := json.Marshal(inventory)
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	old := mechanicalBootstrapRunner
	mechanicalBootstrapRunner = func(_ context.Context, _, model, _ string, _ time.Duration) bool { return model == "challenger" }
	t.Cleanup(func() { mechanicalBootstrapRunner = old })
	if err := RunMechanicalLeaderRace(root); err != nil {
		t.Fatal(err)
	}
	got := readBracketInventory(t, root)["race"].(map[string]any)
	if got["finished"] != true || got["winner"] != "challenger" || len(got["samples"].(map[string]any)["challenger"].([]any)) != mechanicalMinSamples {
		t.Fatalf("built-in leader race did not decide: %#v", got)
	}
}

func TestMechanicalNewcomerBracketAllFailKeepsLeader(t *testing.T) {
	root := mechanicalBracketFixture(t)
	old := mechanicalBootstrapRunner
	mechanicalBootstrapRunner = func(_ context.Context, _, _, _ string, _ time.Duration) bool { return false }
	t.Cleanup(func() { mechanicalBootstrapRunner = old })
	if err := RunMechanicalNewcomerBracket(root); err != nil {
		t.Fatal(err)
	}
	inventory := readBracketInventory(t, root)
	race := inventory["race"].(map[string]any)
	if race["finished"] != true || race["winner"] != "" || race["leader"] != "opencode/leader-free" || measuredLeader(inventory["candidates"].([]any)) != "opencode/leader-free" {
		t.Fatalf("all-fail race=%#v leader=%q", race, measuredLeader(inventory["candidates"].([]any)))
	}
}

func TestMechanicalNewcomerBracketResumesRecordedTask(t *testing.T) {
	root := mechanicalBracketFixture(t)
	if _, _, err := prepareMechanicalBracket(root); err != nil {
		t.Fatal(err)
	}
	if err := recordMechanicalBracketResult(root, "opencode/a-free", 0, true); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	old := mechanicalBootstrapRunner
	mechanicalBootstrapRunner = func(_ context.Context, _, _, _ string, _ time.Duration) bool { calls.Add(1); return true }
	t.Cleanup(func() { mechanicalBootstrapRunner = old })
	if err := RunMechanicalNewcomerBracket(root); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatalf("runner calls=%d, want 5", calls.Load())
	}
}

func mechanicalBracketFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	inventory := map[string]any{
		"candidates": []any{
			map[string]any{"id": "opencode/leader-free", "status": "measured", "mechanical_samples": []any{true, true, true}},
			map[string]any{"id": "opencode/a-free", "status": "unmeasured", "mechanical_samples": []any{}},
			map[string]any{"id": "opencode/b-free", "status": "unmeasured", "mechanical_samples": []any{}},
		},
		"race": map[string]any{"started": true, "finished": false, "stage": "newcomer_bracket", "leader": "opencode/leader-free", "entrants": []any{"opencode/a-free", "opencode/b-free"}, "samples": map[string]any{}, "session_claims": []any{}},
	}
	data, _ := json.Marshal(inventory)
	if err := os.WriteFile(filepath.Join(root, "opencode", "candidates.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func readBracketInventory(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inventory map[string]any
	if err := json.Unmarshal(data, &inventory); err != nil {
		t.Fatal(err)
	}
	return inventory
}
