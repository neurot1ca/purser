package contextio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
)

func activeCoderPath(project, root string) string {
	if root == "" {
		root = stateRoot()
	}
	return filepath.Join(root, "projects", repositoryProjectKey(resolve(project)), "active-coder.json")
}

func defaultActiveCoderState() map[string]any {
	return map[string]any{"active_coder": "primary", "triggering_evidence": nil, "reset_cycle_marker": nil, "last_probe_time": nil}
}

func validateActiveCoderState(value map[string]any) error {
	if len(value) != 4 {
		return errors.New("active-coder state has invalid fields")
	}
	for _, key := range []string{"active_coder", "triggering_evidence", "reset_cycle_marker", "last_probe_time"} {
		if _, ok := value[key]; !ok {
			return errors.New("active-coder state has invalid fields")
		}
	}
	if value["active_coder"] != "primary" && value["active_coder"] != "fallback" {
		return errors.New("active-coder state has an invalid active coder")
	}
	if evidence := value["triggering_evidence"]; evidence != nil {
		switch evidence.(type) {
		case string, map[string]any:
		default:
			return errors.New("active-coder state has invalid triggering evidence")
		}
	}
	for _, key := range []string{"reset_cycle_marker", "last_probe_time"} {
		if v := value[key]; v != nil {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("active-coder state has an invalid %s", key)
			}
		}
	}
	return nil
}

func readActiveCoderState(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultActiveCoderState(), nil
	}
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("active-coder state has trailing data")
		}
		return nil, err
	}
	if err := validateActiveCoderState(value); err != nil {
		return nil, err
	}
	return value, nil
}

func writeActiveCoderState(path string, value map[string]any) error {
	if err := validateActiveCoderState(value); err != nil {
		return err
	}
	return writeJSONAtomically(path, value)
}

func updateActiveCoderState(path string, change func(map[string]any)) (map[string]any, error) {
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	value, err := readActiveCoderState(path)
	if err != nil {
		return nil, err
	}
	change(value)
	if err := writeActiveCoderState(path, value); err != nil {
		return nil, err
	}
	return value, nil
}

func readProjectActiveCoder(project, root string) (map[string]any, error) {
	path := activeCoderPath(project, root)
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readActiveCoderState(path)
}

func writeProjectActiveCoder(project, root string, value map[string]any) error {
	path := activeCoderPath(project, root)
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return writeActiveCoderState(path, value)
}

// ApplyFallbackConsent applies a current, shown answer while holding the active-coder lock.
// A stale answer cannot approve or decline a newer question.
func ApplyFallbackConsent(project, root, session string, approve bool, resetMarker, switchedAt any) (bool, error) {
	changed := false
	_, err := updateActiveCoderState(activeCoderPath(project, root), func(state map[string]any) {
		evidence, ok := state["triggering_evidence"].(map[string]any)
		if state["active_coder"] != "primary" || !ok || evidence["question_session"] != session ||
			evidence["question_shown"] != true || evidence["answerable"] != true {
			return
		}
		if !approve {
			state["triggering_evidence"] = map[string]any{
				"declined": true, "trigger_id": evidence["trigger_id"], "type": evidence["type"],
			}
			changed = true
			return
		}
		state["active_coder"], state["reset_cycle_marker"] = "fallback", resetMarker
		state["last_probe_time"] = switchedAt
		for _, key := range []string{"question", "question_session", "question_shown", "answerable"} {
			delete(evidence, key)
		}
		evidence["switched_at"] = switchedAt
		changed = true
	})
	return changed, err
}

// UpdateFallbackTrigger mutates fallback state only while the triggering failure is current.
func UpdateFallbackTrigger(project, root, kind string, triggerID any, change func(map[string]any)) (bool, error) {
	changed := false
	_, err := updateActiveCoderState(activeCoderPath(project, root), func(state map[string]any) {
		evidence, ok := state["triggering_evidence"].(map[string]any)
		if state["active_coder"] != "fallback" || !ok || evidence["type"] != kind || !reflect.DeepEqual(evidence["trigger_id"], triggerID) {
			return
		}
		change(state)
		changed = true
	})
	return changed, err
}

// ClaimFallbackProbe atomically claims a due probe for the current trigger.
func ClaimFallbackProbe(project, root, kind string, triggerID any, now float64) (bool, error) {
	interval := float64(15 * 60)
	if kind == "NO_RESPONSE" {
		interval = 5 * 60
	} else if kind != "CREDIT_EXHAUSTED" {
		return false, nil
	}
	claimed := false
	_, err := UpdateFallbackTrigger(project, root, kind, triggerID, func(state map[string]any) {
		last := interval
		if raw, ok := state["last_probe_time"].(string); ok {
			if parsed, parseErr := strconv.ParseFloat(raw, 64); parseErr == nil {
				last = now - parsed
			} else {
				last = 0
			}
		}
		if last >= interval {
			state["last_probe_time"] = strconv.FormatFloat(now, 'f', -1, 64)
			claimed = true
		}
	})
	return claimed, err
}

// ApplyPrimaryReturnConsent applies a shown, trigger-bound return answer under the active-coder lock.
func ApplyPrimaryReturnConsent(project, root, session, kind string, triggerID any, approve bool) (bool, error) {
	changed := false
	_, err := UpdateFallbackTrigger(project, root, kind, triggerID, func(state map[string]any) {
		evidence := state["triggering_evidence"].(map[string]any)
		if evidence["question_session"] != session || evidence["question_shown"] != true || evidence["answerable"] != true {
			return
		}
		if approve {
			state["active_coder"], state["triggering_evidence"] = "primary", nil
			state["reset_cycle_marker"], state["last_probe_time"] = nil, nil
		} else {
			marker := evidence["question_reset_marker"]
			for _, key := range []string{"question", "question_session", "question_shown", "answerable", "question_reset_marker"} {
				delete(evidence, key)
			}
			evidence["declined_reset_cycle_marker"] = marker
		}
		changed = true
	})
	return changed, err
}

// RecordPrimaryReturnQuestion stores a quota renewal offer only for the current trigger.
func RecordPrimaryReturnQuestion(project, root, kind string, triggerID any, session, question string, resetMarker any) (bool, error) {
	recorded := false
	_, err := UpdateFallbackTrigger(project, root, kind, triggerID, func(state map[string]any) {
		evidence := state["triggering_evidence"].(map[string]any)
		if hasQuestion, _ := evidence["question_session"].(string); hasQuestion != "" || session == "" {
			return
		}
		evidence["question"], evidence["question_session"] = question, session
		evidence["question_reset_marker"] = resetMarker
		evidence["question_shown"], evidence["answerable"] = false, false
		recorded = true
	})
	return recorded, err
}

// ApplyPrimaryReturnCooldown returns to primary once the current rate-limit cooldown expires.
func ApplyPrimaryReturnCooldown(project, root string, triggerID any, now float64) (bool, error) {
	changed := false
	_, err := UpdateFallbackTrigger(project, root, "RATE_LIMITED", triggerID, func(state map[string]any) {
		evidence := state["triggering_evidence"].(map[string]any)
		delay, ok := evidence["retryAfterSeconds"].(float64)
		if !ok || delay < 0 {
			delay = 5 * 60
		}
		started, parseErr := strconv.ParseFloat(fmt.Sprint(evidence["switched_at"]), 64)
		if parseErr != nil || now < started+delay {
			return
		}
		state["active_coder"], state["triggering_evidence"] = "primary", nil
		state["reset_cycle_marker"], state["last_probe_time"] = nil, nil
		changed = true
	})
	return changed, err
}
