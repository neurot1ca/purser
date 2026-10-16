package enforce

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/proc"
)

const mechanicalMinSamples = 3
const mechanicalMaxSamples = 10

// PostMechanicalLearning mirrors codex.py's optional, fail-open post processing.
func PostMechanicalLearning(config map[string]any, root, pluginRoot, projectKey, session string, pending, bundle map[string]any) {
	if config["mechanical_opencode_enabled"] == false {
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
	if pending["mechanical_opencode"] == true && pending["agent"] == "opencode" {
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
	repository := pending["worktree_path"]
	if repository == nil || repository == "" {
		repository = pending["repo_root"]
	}
	served := ""
	if pending["agent"] == "opencode" {
		served = fmt.Sprint(pending["model"])
	}
	prompt := fmt.Sprint(pending["shadow_prompt"])
	if pending["mechanical_opencode"] != true {
		prompt = mechanicalVerificationPrompt(bundle)
	}
	if session == "" {
		session = stringValue(pending["thread_id"])
	}
	_, _ = startMechanicalShadow(root, projectKey, fmt.Sprint(repository), prompt, fmt.Sprint(bundle["implementer_report"]), served, session)
}

// MechanicalScore returns the Laplace-smoothed success rate once measured.
func MechanicalScore(samples []bool) (float64, bool) {
	if len(samples) < mechanicalMinSamples {
		return 0, false
	}
	successes := 0
	for _, success := range samples {
		if success {
			successes++
		}
	}
	return float64(successes+1) / float64(len(samples)+2), true
}

func mechanicalVerificationPrompt(bundle map[string]any) string {
	paths := anyStringList(bundle["changed_paths"])
	return "Read-only mechanical verification. Inspect the accepted change and report whether it satisfies the implementer report. Do not modify files. Changed paths: " + strings.Join(paths, ", ") + "\nAccepted report:\n" + fmt.Sprint(bundle["implementer_report"])
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
	candidates, ok := inventory["candidates"].([]any)
	if !ok {
		return fmt.Errorf("invalid candidate inventory")
	}
	varCandidate := map[string]any(nil)
	retired := false
	for _, item := range candidates {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := c["id"].(string)
		if id == model {
			varCandidate = c
		}
	}
	if varCandidate == nil {
		if history, ok := inventory["retired_candidates"].([]any); ok {
			for _, item := range history {
				c, ok := item.(map[string]any)
				if ok && c["id"] == model {
					varCandidate, retired = c, true
					break
				}
			}
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
	if retired {
		data, err = json.Marshal(inventory)
		if err != nil {
			return err
		}
		return atomicWrite(path, data)
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
		xs, xMeasured := MechanicalScore(anyBools(x))
		ys, yMeasured := MechanicalScore(anyBools(y))
		return xMeasured && yMeasured && xs > ys
	})
	inventory["candidates"] = candidates
	data, err = json.Marshal(inventory)
	if err != nil {
		return err
	}
	if err = atomicWrite(path, data); err != nil {
		return err
	}
	return nil
}

// ReconcileMechanicalInventory applies a validated successful free-model snapshot.
// Callers must pass only free-model IDs; one invalid ID rejects the whole snapshot.
// IDs are kept exact; the date is the snapshot's UTC catalog date.
func ReconcileMechanicalInventory(root string, modelIDs []string, catalogDate string) error {
	if _, err := time.Parse("2006-01-02", catalogDate); err != nil || catalogDate == "" {
		return fmt.Errorf("invalid catalog date")
	}
	if len(modelIDs) == 0 {
		return fmt.Errorf("empty model catalog")
	}
	seen := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		if !opencodeCandidatePattern.MatchString(id) || seen[id] {
			return fmt.Errorf("invalid model catalog")
		}
		seen[id] = true
	}
	path, unlock, err := lockCandidateInventory(root)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			candidates := make([]any, 0, len(modelIDs))
			for _, id := range modelIDs {
				candidates = append(candidates, map[string]any{
					"id":                 id,
					"status":             "unmeasured",
					"mechanical_samples": []any{},
				})
			}
			inventory := map[string]any{
				"candidates":         candidates,
				"retired_candidates": []any{},
				"catalog_date":       catalogDate,
				"catalog_baseline":   catalogDate,
			}
			data, err = json.Marshal(inventory)
			if err == nil {
				err = atomicWrite(path, data)
			}
			return err
		}
		return err
	}
	var inventory map[string]any
	if err = json.Unmarshal(data, &inventory); err != nil || inventory == nil {
		return fmt.Errorf("invalid candidate inventory")
	}
	active, ok := inventory["candidates"].([]any)
	if !ok {
		return fmt.Errorf("invalid candidate inventory")
	}
	history, exists := inventory["retired_candidates"]
	retired := []any{}
	if exists {
		retired, ok = history.([]any)
		if !ok {
			return fmt.Errorf("invalid candidate history")
		}
	}
	activeByID, historyByID := map[string]map[string]any{}, map[string]map[string]any{}
	for _, group := range []struct {
		items []any
		index map[string]map[string]any
	}{{active, activeByID}, {retired, historyByID}} {
		for _, raw := range group.items {
			candidate, ok := raw.(map[string]any)
			id, idOK := candidate["id"].(string)
			if !ok || !idOK || !opencodeCandidatePattern.MatchString(id) || activeByID[id] != nil || historyByID[id] != nil {
				return fmt.Errorf("invalid candidate inventory entry")
			}
			group.index[id] = candidate
		}
	}
	_, hasBaseline := inventory["catalog_baseline"]
	legacyInventory := !hasBaseline
	if legacyInventory {
		inventory["catalog_baseline"] = catalogDate
		hasBaseline = true
	}
	newActive := make([]any, 0, len(modelIDs))
	newcomers := make([]any, 0, len(modelIDs))
	for _, id := range modelIDs {
		candidate := activeByID[id]
		existed := candidate != nil
		if candidate == nil {
			candidate = historyByID[id]
			if candidate != nil {
				delete(historyByID, id)
			}
		}
		if candidate == nil {
			candidate = map[string]any{"id": id, "status": "unmeasured", "mechanical_samples": []any{}}
			if hasBaseline {
				newcomers = append(newcomers, id)
			}
		}
		if legacyInventory && existed && candidate["status"] == "unmeasured" {
			samples, _ := candidate["mechanical_samples"].([]any)
			if len(anyBools(samples)) == 0 {
				newcomers = append(newcomers, id)
			}
		}
		newActive = append(newActive, candidate)
	}
	newHistory := make([]any, 0, len(retired)+len(active))
	for _, raw := range retired {
		candidate := raw.(map[string]any)
		if _, activeNow := seen[candidate["id"].(string)]; !activeNow {
			newHistory = append(newHistory, candidate)
		}
	}
	for _, raw := range active {
		candidate := raw.(map[string]any)
		if _, activeNow := seen[candidate["id"].(string)]; !activeNow {
			newHistory = append(newHistory, candidate)
		}
	}
	inventory["candidates"], inventory["retired_candidates"], inventory["catalog_date"] = newActive, newHistory, catalogDate
	if len(newcomers) > 0 {
		if _, raceOpen := inventory["race"].(map[string]any); !raceOpen {
			leader := measuredLeader(newActive)
			if leader != "" {
				entrants := append([]any{}, newcomers...)
				stage := "newcomer_bracket"
				if len(entrants) == 1 {
					stage = "newcomer_vs_leader"
					entrants = append(entrants, leader)
				}
				inventory["race"] = map[string]any{"started": true, "finished": false, "started_at": catalogDate, "entrants": entrants, "stage": stage, "leader": leader, "samples": map[string]any{}, "winner": "", "session_claims": []any{}, "notice_claim_ids": []any{}}
			}
		}
	}
	data, err = json.Marshal(inventory)
	if err != nil {
		return err
	}
	if err = atomicWrite(path, data); err != nil {
		return err
	}
	if race, ok := inventory["race"].(map[string]any); ok {
		_ = syncMechanicalRaceLeader(root, inventory, race)
	}
	return nil
}

func measuredLeader(candidates []any) string {
	for _, raw := range candidates {
		if c, ok := raw.(map[string]any); ok && c["status"] == "measured" {
			if id, ok := c["id"].(string); ok {
				return id
			}
		}
	}
	return ""
}

func recordMechanicalRaceSample(root, model string, success bool) error {
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
	race, ok := inventory["race"].(map[string]any)
	if !ok || race["finished"] == true {
		return nil
	}
	entrants, _ := race["entrants"].([]any)
	if race["stage"] == "newcomer_bracket" && len(entrants) > 2 {
		entrants = entrants[:2]
	}
	if !containsAnyString(entrants, model) {
		return nil
	}
	samples, _ := race["samples"].(map[string]any)
	if samples == nil {
		samples = map[string]any{}
	}
	values, _ := samples[model].([]any)
	values = append(values, success)
	if len(values) > mechanicalMaxSamples {
		values = values[:mechanicalMaxSamples]
	}
	samples[model], race["samples"] = values, samples
	advanceMechanicalRace(root, inventory, race)
	data, err = json.Marshal(inventory)
	if err != nil {
		return err
	}
	if err = atomicWrite(path, data); err != nil {
		return err
	}
	_ = syncMechanicalRaceLeader(root, inventory, race)
	return nil
}

func containsAnyString(values []any, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func advanceMechanicalRace(root string, inventory map[string]any, race map[string]any) {
	entrants, _ := race["entrants"].([]any)
	samples, _ := race["samples"].(map[string]any)
	stage, _ := race["stage"].(string)
	pair := entrants
	if stage == "newcomer_bracket" && len(pair) > 2 {
		pair = pair[:2]
	}
	if len(pair) != 2 {
		return
	}
	left, _ := pair[0].(string)
	right, _ := pair[1].(string)
	if stage == "newcomer_vs_leader" {
		leader := mechanicalRaceLeader(inventory, race)
		challenger := left
		if challenger == leader {
			challenger = right
		}
		values, _ := samples[challenger].([]any)
		leaderScore, leaderOK := mechanicalRaceScore(inventory, race, leader)
		challengerScore, challengerOK := mechanicalRaceScore(inventory, race, challenger)
		if !leaderOK || !challengerOK || len(values) < mechanicalMinSamples {
			return
		}
		if challengerScore > leaderScore {
			race["pair_winner"] = challenger
			race["winner"], race["finished"], race["finished_at"] = challenger, true, time.Now().UTC().Format(time.RFC3339)
			return
		}
		successes := 0
		for _, sample := range values {
			if sample == true {
				successes++
			}
		}
		maxScore := float64(successes+mechanicalMaxSamples-len(values)+1) / float64(mechanicalMaxSamples+2)
		if len(values) >= mechanicalMaxSamples || maxScore < leaderScore {
			race["pair_winner"] = leader
			race["winner"], race["finished"], race["finished_at"] = leader, true, time.Now().UTC().Format(time.RFC3339)
		}
		return
	}
	lsScore, lok := mechanicalRaceScore(inventory, race, left)
	rsScore, rok := mechanicalRaceScore(inventory, race, right)
	if !lok || !rok {
		return
	}
	leftSamples, _ := samples[left].([]any)
	rightSamples, _ := samples[right].([]any)
	if stage == "newcomer_bracket" {
		if len(leftSamples) < mechanicalMinSamples || len(rightSamples) < mechanicalMinSamples {
			return
		}
		winner := ""
		if lsScore > rsScore {
			winner = left
		} else if rsScore > lsScore {
			winner = right
		}
		if winner != "" {
			other, otherSamples, winnerScore := left, leftSamples, rsScore
			if winner == left {
				other, otherSamples, winnerScore = right, rightSamples, lsScore
			}
			successes := 0
			for _, sample := range otherSamples {
				if sample == true {
					successes++
				}
			}
			maxScore := float64(successes+mechanicalMaxSamples-len(otherSamples)+1) / float64(mechanicalMaxSamples+2)
			if (maxScore > winnerScore || (maxScore == winnerScore && other == left && winner == right)) && (len(leftSamples) < mechanicalMaxSamples || len(rightSamples) < mechanicalMaxSamples) {
				return
			}
		} else if len(leftSamples) < mechanicalMaxSamples || len(rightSamples) < mechanicalMaxSamples {
			return
		}
	}
	winner, loser := left, right
	if rsScore > lsScore {
		winner, loser = right, left
	}
	if rsScore == lsScore && stage != "newcomer_bracket" {
		winner = ""
	}
	race["pair_winner"] = winner
	if stage == "newcomer_bracket" {
		remaining := append([]any{}, entrants[2:]...)
		if len(remaining) > 0 {
			results, _ := race["completed_stages"].([]any)
			race["completed_stages"] = append(results, map[string]any{"stage": stage, "entrants": append([]any{}, pair...), "samples": samples, "winner": winner})
			if winner == "" {
				winner = left
			}
			race["entrants"] = append([]any{winner}, remaining...)
			race["samples"] = map[string]any{}
			return
		}
		activeLeader := mechanicalRaceLeader(inventory, race)
		if activeLeader == "" || activeLeader == winner {
			activeLeader = loser
		}
		if winner == "" {
			winner = left
		}
		if winner == activeLeader {
			winner = loser
		}
		race["entrants"] = []any{winner, activeLeader}
		race["stage"] = "newcomer_vs_leader"
		race["samples"] = map[string]any{}
		return
	}
	race["winner"], race["finished"], race["finished_at"] = winner, true, time.Now().UTC().Format(time.RFC3339)
}

func mechanicalRaceScore(inventory map[string]any, race map[string]any, model string) (float64, bool) {
	stage, _ := race["stage"].(string)
	samples, _ := race["samples"].(map[string]any)
	if stage == "newcomer_vs_leader" {
		leader := mechanicalRaceLeader(inventory, race)
		if model == leader {
			for _, raw := range inventory["candidates"].([]any) {
				candidate, _ := raw.(map[string]any)
				if candidate != nil && candidate["id"] == model {
					history, _ := candidate["mechanical_samples"].([]any)
					return MechanicalScore(anyBools(history))
				}
			}
			return 0, false
		}
	}
	values, _ := samples[model].([]any)
	return MechanicalScore(anyBools(values))
}

func syncMechanicalRaceLeader(root string, inventory, race map[string]any) error {
	winner, _ := race["winner"].(string)
	if winner == "" || race["finished"] != true {
		return nil
	}
	entrants, _ := race["entrants"].([]any)
	var opponent string
	for _, raw := range entrants {
		if id, ok := raw.(string); ok && id != winner {
			opponent = id
			break
		}
	}
	wScore, wMeasured := mechanicalRaceScore(inventory, race, winner)
	oScore, oMeasured := mechanicalRaceScore(inventory, race, opponent)
	if opponent == "" || !wMeasured || !oMeasured || wScore <= oScore {
		return nil
	}
	candidates, _ := inventory["candidates"].([]any)
	var candidate map[string]any
	for _, raw := range candidates {
		if c, ok := raw.(map[string]any); ok && c["id"] == winner && c["status"] == "measured" {
			candidate = c
			break
		}
	}
	if candidate == nil || mechanicalRaceLeader(inventory, race) != opponent {
		return nil
	}
	candidateSamples, _ := candidate["mechanical_samples"].([]any)
	successes := 0
	for _, v := range candidateSamples {
		if v == true {
			successes++
		}
	}
	data, err := json.Marshal(map[string]any{"model": winner, "successes": successes, "sample_count": len(candidateSamples), "announced": false})
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(root, "opencode", "leader.json"), data)
}

func lockCandidateInventory(root string) (string, func(), error) {
	path := filepath.Join(root, "opencode", "candidates.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", func() {}, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", func() {}, err
	}
	if info, err := lock.Stat(); err != nil {
		_ = lock.Close()
		return "", func() {}, err
	} else if info.Size() == 0 {
		if _, err = lock.WriteAt([]byte("\x00"), 0); err != nil {
			_ = lock.Close()
			return "", func() {}, err
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for contextio.TryLock(lock) != nil {
		if time.Now().After(deadline) {
			_ = lock.Close()
			return "", func() {}, fmt.Errorf("candidate inventory lock unavailable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return path, func() { contextio.Unlock(lock); _ = lock.Close() }, nil
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

func anyBools(values []any) []bool {
	out := make([]bool, 0, len(values))
	for _, v := range values {
		b, ok := v.(bool)
		if !ok {
			return nil
		}
		out = append(out, b)
	}
	return out
}

func startMechanicalShadow(root, projectKey, repository, prompt, report, served, session string) (bool, error) {
	path, unlockInventory, err := lockCandidateInventory(root)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	var inventory map[string]any
	if err == nil {
		err = json.Unmarshal(data, &inventory)
	}
	if err != nil {
		unlockInventory()
		return false, err
	}
	race, ok := inventory["race"].(map[string]any)
	if !ok || race["finished"] == true {
		unlockInventory()
		return false, nil
	}
	if race["stage"] == "newcomer_bracket" {
		unlockInventory()
		return false, nil
	}
	if session == "" {
		unlockInventory()
		return false, nil
	}
	claims, _ := race["session_claims"].([]any)
	for _, claimed := range claims {
		if claimed == session {
			unlockInventory()
			return false, nil
		}
	}
	entrants, _ := race["entrants"].([]any)
	if race["stage"] == "newcomer_bracket" && len(entrants) > 2 {
		entrants = entrants[:2]
	}
	model := ""
	bestSamples := 0
	bestSet := false
	samples, _ := race["samples"].(map[string]any)
	if race["stage"] == "newcomer_vs_leader" {
		leader := mechanicalRaceLeader(inventory, race)
		for _, raw := range entrants {
			id, ok := raw.(string)
			if ok && id != leader {
				model = id
				break
			}
		}
	} else {
		for _, raw := range entrants {
			id, ok := raw.(string)
			if !ok || id == served {
				continue
			}
			values, _ := samples[id].([]any)
			if !bestSet || len(values) < bestSamples {
				model, bestSamples, bestSet = id, len(values), true
			}
		}
	}
	if model == "" {
		unlockInventory()
		return false, nil
	}
	race["session_claims"] = append(claims, session)
	data, err = json.Marshal(inventory)
	if err == nil {
		err = atomicWrite(path, data)
	}
	unlockInventory()
	if err != nil {
		return false, err
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

func mechanicalRaceLeader(inventory map[string]any, race map[string]any) string {
	if leader, _ := race["leader"].(string); leader != "" {
		return leader
	}
	candidates, _ := inventory["candidates"].([]any)
	return measuredLeader(candidates)
}

func mechanicalID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
