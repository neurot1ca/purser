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
