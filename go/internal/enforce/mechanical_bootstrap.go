package enforce

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"purser-hook/internal/proc"
)

//go:embed bootstrap_tasks
var mechanicalBootstrapFiles embed.FS

var mechanicalBootstrapTasks = []string{"rename_symbol", "config_value", "one_line_fix"}

// RunMechanicalNewcomerBracket measures a pending catalog race with bootstrap tasks.
func RunMechanicalNewcomerBracket(root string) error {
	entrants, results, err := prepareMechanicalBracket(root)
	if err != nil || entrants == nil {
		return err
	}
	jobs := make(chan string)
	var workers sync.WaitGroup
	count := len(entrants)
	if count > 3 {
		count = 3
	}
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for model := range jobs {
				for task := range mechanicalBootstrapTasks {
					if results[model][task] != nil {
						continue
					}
					ok := mechanicalBootstrapRunner(context.Background(), root, model, mechanicalBootstrapTasks[task], mechanicalShadowTimeout)
					_ = recordMechanicalBracketResult(root, model, task, ok)
				}
			}
		}()
	}
	for _, model := range entrants {
		jobs <- model
	}
	close(jobs)
	workers.Wait()
	return finishMechanicalBracket(root)
}

func prepareMechanicalBracket(root string) ([]string, map[string][]*bool, error) {
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		return nil, nil, err
	}
	race, _ := inventory["race"].(map[string]any)
	if race == nil || race["finished"] == true || race["stage"] != "newcomer_bracket" {
		return nil, nil, nil
	}
	entrants, _ := race["entrants"].([]any)
	ids := make([]string, 0, len(entrants))
	for _, raw := range entrants {
		if id, ok := raw.(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("invalid mechanical bracket entrants")
	}
	results, _ := race["bootstrap_results"].(map[string]any)
	if results == nil {
		results = map[string]any{}
	}
	state := map[string][]*bool{}
	for _, id := range ids {
		if raw, ok := results[id]; ok {
			encoded, _ := json.Marshal(raw)
			var values []*bool
			if err = json.Unmarshal(encoded, &values); err != nil || len(values) != len(mechanicalBootstrapTasks) {
				return nil, nil, fmt.Errorf("invalid mechanical bracket results")
			}
			state[id] = values
		} else {
			state[id] = make([]*bool, len(mechanicalBootstrapTasks))
			results[id] = state[id]
		}
	}
	race["bootstrap_results"] = results
	if err = writeBootstrapInventory(path, inventory); err != nil {
		return nil, nil, err
	}
	return ids, state, nil
}

func recordMechanicalBracketResult(root, model string, task int, success bool) error {
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		return err
	}
	race, _ := inventory["race"].(map[string]any)
	if race == nil || race["stage"] != "newcomer_bracket" || race["finished"] == true {
		return nil
	}
	results, _ := race["bootstrap_results"].(map[string]any)
	state := map[string][]*bool{}
	for id, raw := range results {
		encoded, _ := json.Marshal(raw)
		var values []*bool
		_ = json.Unmarshal(encoded, &values)
		state[id] = values
	}
	if task < 0 || task >= len(mechanicalBootstrapTasks) || state[model] == nil || state[model][task] != nil {
		return nil
	}
	result := success
	state[model][task] = &result
	race["bootstrap_results"] = state
	return writeBootstrapInventory(path, inventory)
}

func finishMechanicalBracket(root string) error {
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		return err
	}
	race, _ := inventory["race"].(map[string]any)
	if race == nil || race["stage"] != "newcomer_bracket" || race["finished"] == true {
		return nil
	}
	entrants, _ := race["entrants"].([]any)
	results, _ := race["bootstrap_results"].(map[string]any)
	order := map[string]int{}
	if candidates, ok := inventory["candidates"].([]any); ok {
		for i, raw := range candidates {
			c, _ := raw.(map[string]any)
			if c != nil {
				if id, _ := c["id"].(string); id != "" {
					order[id] = i
				}
			}
		}
	}
	best, bestScore, bestSuccesses := "", -1.0, -1
	for _, raw := range entrants {
		id, _ := raw.(string)
		var samples []*bool
		encoded, _ := json.Marshal(results[id])
		if err = json.Unmarshal(encoded, &samples); err != nil || len(samples) != len(mechanicalBootstrapTasks) {
			return fmt.Errorf("mechanical bracket result incomplete")
		}
		successes := 0
		for _, sample := range samples {
			if sample == nil {
				return fmt.Errorf("mechanical bracket result incomplete")
			}
			if *sample {
				successes++
			}
		}
		if successes == 0 {
			continue
		}
		score := mechanicalBootstrapScore(successes, len(samples))
		if score > bestScore || score == bestScore && (successes > bestSuccesses || successes == bestSuccesses && order[id] < order[best]) {
			best, bestScore, bestSuccesses = id, score, successes
		}
	}
	leader := mechanicalRaceLeader(inventory, race)
	if best == "" {
		race["winner"], race["finished"], race["finished_at"] = "", true, time.Now().UTC().Format(time.RFC3339)
	} else {
		if leader == "" || best == leader {
			return fmt.Errorf("mechanical bracket leader missing")
		}
		race["entrants"], race["stage"], race["samples"], race["winner"] = []any{best, leader}, "newcomer_vs_leader", map[string]any{}, ""
	}
	return writeBootstrapInventory(path, inventory)
}

type mechanicalBootstrapState struct {
	Status     string             `json:"status"`
	StartedAt  string             `json:"started_at"`
	FinishedAt string             `json:"finished_at,omitempty"`
	Entrants   []string           `json:"entrants"`
	Winner     string             `json:"winner,omitempty"`
	Results    map[string][]*bool `json:"results"`
}

// BootstrapMechanicalRace measures every active candidate only while no measured leader exists.
func BootstrapMechanicalRace(root string) error {
	state, err := prepareMechanicalBootstrap(root)
	if err != nil || state.Status == "finished" || state.Status == "measured" {
		return err
	}
	jobs := make(chan string)
	var workers sync.WaitGroup
	count := len(state.Entrants)
	if count > 3 {
		count = 3
	}
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for model := range jobs {
				for i, task := range mechanicalBootstrapTasks {
					state, e := readMechanicalBootstrap(root)
					if e != nil || state.Results[model] == nil || state.Results[model][i] != nil {
						continue
					}
					ok := mechanicalBootstrapRunner(context.Background(), root, model, task, mechanicalShadowTimeout)
					_ = recordMechanicalBootstrapResult(root, model, i, ok)
				}
			}
		}()
	}
	for _, model := range state.Entrants {
		jobs <- model
	}
	close(jobs)
	workers.Wait()
	return finishMechanicalBootstrap(root)
}

func prepareMechanicalBootstrap(root string) (mechanicalBootstrapState, error) {
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return mechanicalBootstrapState{}, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return mechanicalBootstrapState{}, err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		return mechanicalBootstrapState{}, err
	}
	var state mechanicalBootstrapState
	if raw, ok := inventory["bootstrap"]; ok {
		encoded, _ := json.Marshal(raw)
		if err = json.Unmarshal(encoded, &state); err != nil {
			return state, err
		}
		if state.Status == "finished" {
			return state, nil
		}
		if state.Status != "started" || state.Results == nil {
			return state, fmt.Errorf("invalid bootstrap state")
		}
		for _, id := range state.Entrants {
			if len(state.Results[id]) != len(mechanicalBootstrapTasks) {
				return state, fmt.Errorf("invalid bootstrap results")
			}
		}
	} else {
		candidates, ok := inventory["candidates"].([]any)
		if !ok {
			return state, fmt.Errorf("invalid candidate inventory")
		}
		entrants := []string{}
		measured := false
		for _, raw := range candidates {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if c["status"] == "measured" {
				measured = true
			}
			id, _ := c["id"].(string)
			if id != "" && c["status"] != "failed" {
				entrants = append(entrants, id)
			}
		}
		if measured {
			return mechanicalBootstrapState{Status: "measured"}, nil
		}
		state = mechanicalBootstrapState{Status: "started", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Entrants: entrants, Results: map[string][]*bool{}}
		for _, id := range entrants {
			state.Results[id] = make([]*bool, len(mechanicalBootstrapTasks))
		}
		inventory["bootstrap"] = state
		if err = writeBootstrapInventory(path, inventory); err != nil {
			return state, err
		}
	}
	return state, nil
}

func readMechanicalBootstrap(root string) (mechanicalBootstrapState, error) {
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return mechanicalBootstrapState{}, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return mechanicalBootstrapState{}, err
	}
	var inventory struct {
		Bootstrap mechanicalBootstrapState `json:"bootstrap"`
	}
	err = json.Unmarshal(data, &inventory)
	return inventory.Bootstrap, err
}

func recordMechanicalBootstrapResult(root, model string, task int, success bool) error {
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		return err
	}
	var state mechanicalBootstrapState
	raw, _ := json.Marshal(inventory["bootstrap"])
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if task < 0 || task >= len(mechanicalBootstrapTasks) || state.Results[model] == nil || state.Results[model][task] != nil {
		return nil
	}
	candidates, ok := inventory["candidates"].([]any)
	if !ok {
		return fmt.Errorf("invalid candidate inventory")
	}
	var candidate map[string]any
	for _, item := range candidates {
		c, _ := item.(map[string]any)
		if c["id"] == model {
			candidate = c
			break
		}
	}
	if candidate == nil {
		result := false
		state.Results[model][task] = &result
		inventory["bootstrap"] = state
		return writeBootstrapInventory(path, inventory)
	}
	markers, _ := candidate["bootstrap_samples"].([]any)
	key := fmt.Sprintf("%s/%d", model, task)
	seen := false
	for _, marker := range markers {
		if marker == key {
			seen = true
		}
	}
	if !seen {
		samples, ok := candidate["mechanical_samples"].([]any)
		if !ok || !boolSamples(samples) {
			samples = []any{}
		}
		samples = append(samples, success)
		if len(samples) > 20 {
			samples = samples[len(samples)-20:]
		}
		candidate["mechanical_samples"] = samples
		if len(samples) >= len(mechanicalBootstrapTasks) && candidate["status"] != "failed" {
			candidate["status"] = "measured"
		}
		candidate["bootstrap_samples"] = append(markers, key)
	}
	result := success
	state.Results[model][task] = &result
	inventory["bootstrap"] = state
	sortMechanicalBootstrapCandidates(candidates)
	return writeBootstrapInventory(path, inventory)
}

func sortMechanicalBootstrapCandidates(candidates []any) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, _ := candidates[i].(map[string]any)
		b, _ := candidates[j].(map[string]any)
		if a == nil || b == nil {
			return a != nil
		}
		if (a["status"] == "measured") != (b["status"] == "measured") {
			return a["status"] == "measured"
		}
		return mechanicalBootstrapCandidateScore(a) > mechanicalBootstrapCandidateScore(b)
	})
}

func mechanicalBootstrapCandidateScore(candidate map[string]any) float64 {
	samples, _ := candidate["mechanical_samples"].([]any)
	successes := 0
	for _, sample := range samples {
		if sample == true {
			successes++
		}
	}
	return mechanicalBootstrapScore(successes, len(samples))
}

func finishMechanicalBootstrap(root string) error {
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		return err
	}
	var state mechanicalBootstrapState
	raw, _ := json.Marshal(inventory["bootstrap"])
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.Status == "finished" {
		return nil
	}
	for _, id := range state.Entrants {
		for _, result := range state.Results[id] {
			if result == nil {
				return fmt.Errorf("bootstrap result incomplete")
			}
		}
	}
	active := make(map[string]bool)
	if candidates, ok := inventory["candidates"].([]any); ok {
		for _, item := range candidates {
			candidate, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := candidate["id"].(string)
			if id != "" {
				active[id] = true
			}
		}
	}
	best, bestScore := "", -1.0
	for _, id := range state.Entrants {
		if !active[id] {
			continue
		}
		successes := 0
		for _, result := range state.Results[id] {
			if *result {
				successes++
			}
		}
		score := mechanicalBootstrapScore(successes, len(mechanicalBootstrapTasks))
		if score > bestScore {
			best, bestScore = id, score
		}
	}
	state.Status, state.FinishedAt, state.Winner = "finished", time.Now().UTC().Format(time.RFC3339Nano), best
	inventory["bootstrap"] = state
	return writeBootstrapInventory(path, inventory)
}

// mechanicalBootstrapScore mirrors the M3 smoothed score until its shared helper lands.
func mechanicalBootstrapScore(successes, samples int) float64 {
	return float64(successes+1) / float64(samples+2)
}

func writeBootstrapInventory(path string, inventory map[string]any) error {
	data, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

var mechanicalBootstrapRunner = runMechanicalBootstrapTask

func runMechanicalBootstrapTask(ctx context.Context, root, model, task string, timeout time.Duration) bool {
	base := "bootstrap_tasks/" + task
	input, err := fs.Sub(mechanicalBootstrapFiles, base+"/input")
	if err != nil {
		return false
	}
	expected, err := fs.Sub(mechanicalBootstrapFiles, base+"/expected")
	if err != nil {
		return false
	}
	scratch, err := os.MkdirTemp(filepath.Join(root, "opencode"), "bootstrap-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(scratch)
	if copyEmbeddedTree(input, scratch) != nil {
		return false
	}
	if timeout <= 0 {
		timeout = mechanicalShadowTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executable, err := exec.LookPath("opencode")
	if err != nil {
		return false
	}
	description, err := mechanicalBootstrapFiles.ReadFile(base + "/task.txt")
	if err != nil {
		return false
	}
	permissions := map[string]string{"edit": "allow", "write": "allow", "bash": "allow", "webfetch": "deny", "task": "deny", "external_directory": "deny"}
	config, _ := json.Marshal(map[string]any{"permission": permissions, "agent": map[string]any{"purser-bootstrap": map[string]any{"mode": "primary", "permission": permissions}}})
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := proc.Hide(exec.CommandContext(commandCtx, executable, "run", "--format", "json", "--model", model, "--agent", "purser-bootstrap", "--dir", scratch, string(description)))
	cmd.Dir = scratch
	cmd.Env = append(os.Environ(), "OPENCODE_CONFIG_CONTENT="+string(config))
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	proc.ConfigureProcessTreeKill(cmd)
	if cmd.Run() != nil || commandCtx.Err() != nil {
		return false
	}
	return equalEmbeddedTree(scratch, expected)
}

func copyEmbeddedTree(source fs.FS, destination string) error {
	return fs.WalkDir(source, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(path))
		if entry.IsDir() {
			if path == "." {
				return nil
			}
			return os.MkdirAll(target, 0700)
		}
		data, err := fs.ReadFile(source, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	})
}

func equalEmbeddedTree(actual string, expected fs.FS) bool {
	want := map[string][]byte{}
	if fs.WalkDir(expected, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, e := fs.ReadFile(expected, path)
			if e != nil {
				return e
			}
			want[path] = data
		}
		return nil
	}) != nil {
		return false
	}
	got := map[string][]byte{}
	if filepath.WalkDir(actual, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(actual, path)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if strings.HasPrefix(firstPathSegment(relSlash), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		got[relSlash] = data
		return nil
	}) != nil {
		return false
	}
	if len(got) != len(want) {
		return false
	}
	for name, data := range want {
		if normalizeBootstrapFile(got[name]) != normalizeBootstrapFile(data) {
			return false
		}
	}
	return true
}

func normalizeBootstrapFile(data []byte) string {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.TrimRightFunc(text, unicode.IsSpace)
}

func firstPathSegment(path string) string {
	if index := strings.IndexByte(path, '/'); index >= 0 {
		return path[:index]
	}
	return path
}
