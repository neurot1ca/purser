package lease

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"purser-hook/internal/contextio"
)

// Lock acquires a scope-leases lock file and returns its release function.
func Lock(path string) (func() error, error) { return contextio.LockFile(path) }

// Validate checks a serialized scope_leases.json record using Python's _read rules.
func Validate(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return errors.New("scope lease record is invalid")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("scope lease record is invalid")
	}
	record, ok := value.(map[string]any)
	if !ok || !pythonOne(record["version"]) {
		return errors.New("scope lease record is invalid")
	}
	owners, ok := record["owners"].(map[string]any)
	if !ok {
		return errors.New("scope lease record is invalid")
	}
	if conflicts, exists := record["conflicts"]; exists {
		items, ok := conflicts.([]any)
		if !ok {
			return errors.New("scope lease conflicts are invalid")
		}
		for _, item := range items {
			conflict, ok := item.(map[string]any)
			if !ok || nonemptyString(conflict["path"]) == false || !pythonNumber(conflict["recorded_at"]) {
				return errors.New("scope lease conflicts are invalid")
			}
		}
	}
	for _, rawOwner := range owners {
		owner, ok := rawOwner.(map[string]any)
		if !ok || !oneOf(owner["state"], "ACTIVE", "QUEUED", "RELEASED") ||
			!isString(owner["task_id"]) {
			return errors.New("scope lease owner is invalid")
		}
		scope, ok := owner["scope"].([]any)
		if !ok || (len(scope) == 0 && owner["read_only"] != true) ||
			(owner["read_only"] == true && len(scope) != 0) {
			return errors.New("scope lease owner is invalid")
		}
		for _, path := range scope {
			if !nonemptyString(path) {
				return errors.New("scope lease owner is invalid")
			}
		}
		if owner["state"] == "QUEUED" && (!isString(owner["job_id"]) || !pythonNumber(owner["queued_at"])) {
			return errors.New("scope lease owner is invalid")
		}
	}
	return nil
}

func oneOf(value any, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func isString(value any) bool { _, ok := value.(string); return ok }
func nonemptyString(value any) bool {
	s, ok := value.(string)
	return ok && s != ""
}

func pythonNumber(value any) bool {
	if _, ok := value.(bool); ok {
		return true
	}
	_, ok := value.(json.Number)
	return ok
}

func pythonOne(value any) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	n, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := strconv.ParseFloat(string(n), 64)
	return err == nil && parsed == 1
}
