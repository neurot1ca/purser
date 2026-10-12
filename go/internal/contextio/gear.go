package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var validGearProjectKey = regexp.MustCompile(`^[0-9a-f]{16}$`)

type GearState struct {
	Version int    `json:"version"`
	Gear    string `json:"gear"`
}

type CoderEffortState struct {
	Version int    `json:"version"`
	Effort  string `json:"effort"`
}

func ProjectKey(repository string) string {
	repository = filepath.Clean(repository)
	if runtime.GOOS == "windows" {
		repository = strings.ToLower(repository)
	}
	h := sha256.Sum256([]byte(repository))
	return hex.EncodeToString(h[:])[:16]
}

func Gear(root, projectKey string) (bool, error) {
	if !validGearProjectKey.MatchString(projectKey) {
		return false, errors.New("invalid project key")
	}
	path := filepath.Join(root, "projects", projectKey, "gear.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	return readGear(path)
}

func readGear(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil || state == nil {
		return false, errors.New("gear state is malformed")
	}
	var version int
	var gear string
	if json.Unmarshal(state["version"], &version) != nil || version != 1 ||
		json.Unmarshal(state["gear"], &gear) != nil || gear != "up" {
		return false, errors.New("gear state is malformed")
	}
	return true, nil
}

func SetGear(root, projectKey, gear string) error {
	if !validGearProjectKey.MatchString(projectKey) || (gear != "up" && gear != "down") {
		return errors.New("invalid gear state")
	}
	path := filepath.Join(root, "projects", projectKey, "gear.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if gear == "down" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, err := readGear(path); err != nil {
		return err
	}
	return writeJSONAtomically(path, GearState{Version: 1, Gear: "up"})
}

// CoderEffort returns the project's optional Codex reasoning effort override.
func CoderEffort(root, projectKey string) (string, error) {
	if !validGearProjectKey.MatchString(projectKey) {
		return "", errors.New("invalid project key")
	}
	path := filepath.Join(root, "projects", projectKey, "coder-effort.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var state CoderEffortState
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || !validCoderEffort(state.Effort) {
		return "", errors.New("coder effort state is malformed")
	}
	return state.Effort, nil
}

// SetCoderEffort stores a project override; auto removes it.
func SetCoderEffort(root, projectKey, effort string) error {
	if !validGearProjectKey.MatchString(projectKey) || effort != "auto" && !validCoderEffort(effort) {
		return errors.New("invalid coder effort")
	}
	path := filepath.Join(root, "projects", projectKey, "coder-effort.json")
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if effort == "auto" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeJSONAtomically(path, CoderEffortState{Version: 1, Effort: effort})
}

func validCoderEffort(effort string) bool {
	return effort == "low" || effort == "medium" || effort == "high" || effort == "xhigh"
}
