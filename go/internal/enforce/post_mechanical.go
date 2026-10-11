package enforce

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/proc"
)

const mechanicalMinSamples = 5
const mechanicalShadowTurnSpacing = 70

// PostMechanicalLearning mirrors codex.py's optional, fail-open post processing.
func PostMechanicalLearning(config map[string]any, root, pluginRoot, projectKey string, pending, bundle map[string]any) {
	if config["mechanical_opencode_enabled"] == false {
		return
	}
	turn, eligible, unlock, err := advanceMechanicalShadowTurn(root, projectKey)
	if err == nil {
		defer unlock()
	} else {
		eligible = false
	}
	if pending["mechanical_opencode"] != true {
		return
	}
	skipped, _ := bundle["reviewer1_skipped_reason"].(string)
	validation, _ := bundle["worker_validation"].(map[string]any)
	accepted := skipped == "NO_DELTA" || skipped == "BELOW_THRESHOLD"
	if accepted {
		accepted = workerValidationPassed(validation)
	} else {
		accepted = bundle["reviewer1_skipped_reason"] == nil && workerValidationPassed(validation) &&
			(bundle["validation"] == "PASS" || bundle["validation"] == "UNKNOWN") &&
			bundle["reviewer1_result"] == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"
	}
	accepted = accepted && bundle["validation"] != "FAIL" && validation["status"] != "FAIL"
	accepted = accepted && bundle["micro_repair"] == nil && bundle["micro_repair_cleared_review"] != true
	if pending["agent"] == "opencode" {
		if models, ok := pending["attempted_models"].([]any); ok {
			for _, value := range models {
				if model, ok := value.(string); ok && model != "" && model != pending["model"] {
					if err := recordMechanicalSample(root, model, false); err != nil {
						return
					}
				}
			}
		}
		if err := recordMechanicalSample(root, fmt.Sprint(pending["model"]), accepted); err != nil {
			return
		}
	}
	if !accepted {
		return
	}
	if !eligible {
		return
	}
	repository := pending["worktree_path"]
	if repository == nil || repository == "" {
		repository = pending["repo_root"]
	}
	served := ""
	if pending["agent"] == "opencode" {
		served = fmt.Sprint(pending["model"])
	}
	started, err := startMechanicalShadow(root, projectKey, fmt.Sprint(repository), fmt.Sprint(pending["shadow_prompt"]), fmt.Sprint(bundle["implementer_report"]), served)
	if err == nil && started {
		_ = setMechanicalShadowTurn(root, projectKey, turn)
	}
}

type mechanicalShadowCadence struct {
	TurnCount      int `json:"turn_count"`
	LastShadowTurn int `json:"last_shadow_turn"`
}

func advanceMechanicalShadowTurn(root, projectKey string) (int, bool, func(), error) {
	dir := filepath.Join(root, "projects", projectKey, "telemetry")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, false, func() {}, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "mechanical_shadow_cadence.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 0, false, func() {}, err
	}
	if info, statErr := lock.Stat(); statErr != nil {
		_ = lock.Close()
		return 0, false, func() {}, statErr
	} else if info.Size() == 0 {
		_, _ = lock.WriteAt([]byte("\x00"), 0)
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for contextio.TryLock(lock) != nil {
		if time.Now().After(deadline) {
			_ = lock.Close()
			return 0, false, func() {}, fmt.Errorf("mechanical shadow cadence lock unavailable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	unlock := func() { contextio.Unlock(lock); _ = lock.Close() }
	path := filepath.Join(dir, "mechanical_shadow_cadence.json")
	var state mechanicalShadowCadence
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		if json.Unmarshal(data, &state) != nil || state.TurnCount < 0 || state.LastShadowTurn < 0 || state.LastShadowTurn > state.TurnCount {
			unlock()
			return 0, false, func() {}, fmt.Errorf("invalid mechanical shadow cadence state")
		}
	} else if !os.IsNotExist(readErr) {
		unlock()
		return 0, false, func() {}, readErr
	}
	state.TurnCount++
	data, err = json.Marshal(state)
	if err == nil {
		err = atomicWrite(path, data)
	}
	if err != nil {
		unlock()
		return 0, false, func() {}, err
	}
	eligible := state.TurnCount >= mechanicalShadowTurnSpacing &&
		(state.LastShadowTurn == 0 || state.TurnCount-state.LastShadowTurn >= mechanicalShadowTurnSpacing)
	return state.TurnCount, eligible, unlock, nil
}

func setMechanicalShadowTurn(root, projectKey string, turn int) error {
	path := filepath.Join(root, "projects", projectKey, "telemetry", "mechanical_shadow_cadence.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var state mechanicalShadowCadence
	if err = json.Unmarshal(data, &state); err != nil {
		return err
	}
	state.LastShadowTurn = turn
	data, err = json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func workerValidationPassed(validation map[string]any) bool {
	if validation["status"] != "PASS" {
		return false
	}
	commands, ok := postStringList(validation["commands"])
	if !ok || len(commands) == 0 {
		return false
	}
	exitCodes, ok := validation["exit_codes"].([]any)
	if !ok || len(exitCodes) != len(commands) {
		return false
	}
	for _, code := range exitCodes {
		switch code := code.(type) {
		case json.Number:
			if code.String() != "0" {
				return false
			}
		case float64:
			if code != 0 {
				return false
			}
		case int:
			if code != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func recordMechanicalSample(root, model string, success bool) error {
	path := filepath.Join(root, "opencode", "candidates.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if info, e := lock.Stat(); e != nil {
		return e
	} else if info.Size() == 0 {
		_, _ = lock.WriteAt([]byte("\x00"), 0)
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for contextio.TryLock(lock) != nil {
		if time.Now().After(deadline) {
			return fmt.Errorf("candidate inventory lock unavailable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer contextio.Unlock(lock)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil {
		return err
	}
	candidates, ok := inventory["candidates"].([]any)
	if !ok {
		return fmt.Errorf("invalid candidate inventory")
	}
	oldLeader, varCandidate := "", map[string]any(nil)
	for _, item := range candidates {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := c["id"].(string)
		if oldLeader == "" && c["status"] == "measured" {
			oldLeader = id
		}
		if id == model {
			varCandidate = c
		}
	}
	if varCandidate == nil {
		return nil
	}
	samples, ok := varCandidate["mechanical_samples"].([]any)
	if !ok || !boolSamples(samples) {
		samples = nil
	}
	samples = append(samples, success)
	if len(samples) > 20 {
		samples = samples[len(samples)-20:]
	}
	varCandidate["mechanical_samples"] = samples
	if len(samples) >= mechanicalMinSamples && varCandidate["status"] != "failed" {
		varCandidate["status"] = "measured"
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, _ := candidates[i].(map[string]any)
		b, _ := candidates[j].(map[string]any)
		rank := func(c map[string]any) int {
			if c == nil {
				return 1
			}
			if c["status"] == "failed" {
				return 2
			}
			s, _ := c["mechanical_samples"].([]any)
			if len(s) >= mechanicalMinSamples {
				return 0
			}
			return 1
		}
		ra, rb := rank(a), rank(b)
		if ra != rb {
			return ra < rb
		}
		if ra != 0 {
			return false
		}
		x, _ := a["mechanical_samples"].([]any)
		y, _ := b["mechanical_samples"].([]any)
		count := func(s []any) int {
			n := 0
			for _, v := range s {
				if v == true {
					n++
				}
			}
			return n
		}
		return count(x)*len(y) > count(y)*len(x)
	})
	inventory["candidates"] = candidates
	data, err = json.Marshal(inventory)
	if err != nil {
		return err
	}
	if err = atomicWrite(path, data); err != nil {
		return err
	}
	var leader map[string]any
	for _, item := range candidates {
		c, _ := item.(map[string]any)
		if c == nil {
			continue
		}
		if leader == nil && c["status"] == "measured" {
			leader = c
		}
	}
	if leader != nil && leader["id"] != oldLeader {
		if s, ok := leader["mechanical_samples"].([]any); ok && boolSamples(s) {
			count := 0
			for _, v := range s {
				if v == true {
					count++
				}
			}
			if d, e := json.Marshal(map[string]any{"model": leader["id"], "successes": count, "sample_count": len(s), "announced": false}); e == nil {
				_ = atomicWrite(filepath.Join(filepath.Dir(path), "leader.json"), d)
			}
		}
	}
	return nil
}

func boolSamples(values []any) bool {
	for _, v := range values {
		if _, ok := v.(bool); !ok {
			return false
		}
	}
	return true
}

type mechanicalShadowSpec struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
}

func shadowSpec(executable, job string) mechanicalShadowSpec {
	return mechanicalShadowSpec{[]string{executable, "mechanical-shadow", job}, nil, filepath.Dir(job)}
}

var startMechanicalShadowDetached = proc.StartDetached

func startMechanicalShadow(root, projectKey, repository, prompt, report, served string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
	if err != nil {
		return false, err
	}
	var inventory struct {
		Candidates []map[string]any `json:"candidates"`
	}
	if err = json.Unmarshal(data, &inventory); err != nil {
		return false, err
	}
	model := ""
	for _, c := range inventory.Candidates {
		id, _ := c["id"].(string)
		s, _ := c["mechanical_samples"].([]any)
		if id != "" && id != served && c["status"] != "failed" && len(s) < mechanicalMinSamples {
			model = id
			break
		}
	}
	if model == "" {
		return false, nil
	}
	shadows := filepath.Join(root, "projects", projectKey, "opencode_shadows")
	if err = os.MkdirAll(shadows, 0700); err != nil {
		return false, err
	}
	lock := filepath.Join(shadows, "running.lock")
	lf, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	_, writeErr := lf.WriteString(fmt.Sprint(os.Getpid()))
	closeErr := lf.Close()
	if writeErr != nil {
		_ = os.Remove(lock)
		return false, writeErr
	}
	if closeErr != nil {
		_ = os.Remove(lock)
		return false, closeErr
	}
	id, err := mechanicalID()
	if err != nil {
		_ = os.Remove(lock)
		return false, err
	}
	job := filepath.Join(shadows, id+".json")
	scratchID, err := mechanicalID()
	if err != nil {
		_ = os.Remove(lock)
		return false, err
	}
	scratch := filepath.Join(shadows, "run-"+scratchID)
	jobData, err := json.Marshal(map[string]any{"model": model, "repository": repository, "prompt": prompt, "accepted_report": report, "scratch": scratch, "project_key": projectKey, "state_root": root})
	if err == nil {
		err = atomicWrite(job, jobData)
	}
	if err != nil {
		_ = os.Remove(job)
		_ = os.Remove(lock)
		return false, err
	}
	executable, err := os.Executable()
	if err != nil {
		_ = os.Remove(job)
		_ = os.Remove(lock)
		return false, err
	}
	spec := shadowSpec(executable, job)
	if err = startMechanicalShadowDetached(spec.Argv, spec.Env, spec.Cwd); err != nil {
		_ = os.Remove(job)
		_ = os.Remove(lock)
		return false, err
	}
	return true, nil
}

func mechanicalID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
