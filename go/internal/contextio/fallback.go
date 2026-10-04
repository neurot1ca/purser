package contextio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
