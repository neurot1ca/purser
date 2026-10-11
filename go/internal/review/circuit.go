package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"purser-hook/internal/lease"
)

const reviewerCircuitDuration = 2 * time.Hour

type reviewerCircuit struct {
	Timeouts  int       `json:"timeouts"`
	OpenUntil time.Time `json:"open_until,omitempty"`
}

func reviewerCircuitPath(dir, agent, model string) string {
	key := sha256.Sum256([]byte(agent + "\x00" + model))
	return filepath.Join(dir, "reviewer-circuits", hex.EncodeToString(key[:])+".json")
}

// ReviewerCircuitOpen reports whether the persisted circuit is open. State
// errors fail open so telemetry cannot prevent a reviewer from running.
func ReviewerCircuitOpen(dir, agent, model string, now time.Time) bool {
	path := reviewerCircuitPath(dir, agent, model)
	release, err := lease.Lock(path + ".lock")
	if err != nil {
		return false
	}
	defer release()
	state, err := readReviewerCircuit(path)
	return err == nil && state.Timeouts >= 3 && now.Before(state.OpenUntil)
}

// RecordReviewerCircuitOutcome advances the timeout streak or resets it after
// any non-timeout result. State errors are intentionally ignored.
func RecordReviewerCircuitOutcome(dir, agent, model, outcome string, now time.Time) {
	path := reviewerCircuitPath(dir, agent, model)
	release, err := lease.Lock(path + ".lock")
	if err != nil {
		return
	}
	defer release()
	state, _ := readReviewerCircuit(path)
	if outcome == "REVIEWER_TIMEOUT" {
		state.Timeouts++
		if state.Timeouts >= 3 {
			state.OpenUntil = now.Add(reviewerCircuitDuration)
		}
	} else {
		state = reviewerCircuit{}
	}
	_ = writeReviewerCircuit(path, state)
}

func readReviewerCircuit(path string) (reviewerCircuit, error) {
	var state reviewerCircuit
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func writeReviewerCircuit(path string, state reviewerCircuit) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), path)
}
