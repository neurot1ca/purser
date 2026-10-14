package initsetup

import (
	"bytes"
	"errors"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"purser-hook/internal/contextio"
	"purser-hook/internal/profile"
)

func updateLocked(path string, edit func([]byte) ([]byte, error)) error {
	unlock, err := contextio.LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()

	original, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated, err := edit(original)
	if err != nil || bytes.Equal(original, updated) {
		return err
	}
	if len(original) > 0 {
		if err := backup(path, original); err != nil {
			return err
		}
	}
	return atomicWrite(path, updated, false)
}

func encodeRoleSections(values map[string]any, sections ...string) ([]byte, error) {
	project := map[string]any{"schema_version": 1}
	for _, name := range sections {
		if value, ok := values[name]; ok {
			project[name] = value
		}
	}
	var output bytes.Buffer
	if err := toml.NewEncoder(&output).Encode(project); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func projectRoleText(original []byte, effective map[string]any, sections ...string) ([]byte, error) {
	values := map[string]any{"schema_version": 1}
	if len(original) > 0 {
		parsed, _, err := profile.DecodeTOML(original)
		if err != nil {
			return nil, err
		}
		values = parsed
		values["schema_version"] = 1
		complete := true
		for _, name := range sections {
			if _, exists := values[name]; !exists && effective[name] != nil {
				complete = false
				break
			}
		}
		if complete {
			if _, hasTrust := values["trust"]; !hasTrust {
				return append([]byte(nil), original...), nil
			}
		}
	}
	for _, name := range sections {
		if _, exists := values[name]; !exists {
			if value, ok := effective[name]; ok {
				values[name] = value
			}
		}
	}
	return encodeRoleSections(values, append([]string{"roles", "coding", "reviewer", "fallback", "mechanical"}, sections...)...)
}

func effectiveUserConfig(root, globalPath string) (map[string]any, error) {
	return profile.ResolveUserConfig(profile.StateRoot(), root, globalPath)
}

func editGlobalTrust(path, root string) error {
	return updateLocked(path, func(original []byte) ([]byte, error) {
		text := string(original)
		if text == "" {
			text = "schema_version = 1\n"
		}
		values := map[string]any{}
		if len(original) > 0 {
			var err error
			values, _, err = profile.DecodeTOML(original)
			if err != nil {
				return nil, err
			}
		}
		trust, ok := values["trust"].(map[string]any)
		if !ok {
			trust = map[string]any{}
			values["trust"] = trust
		}
		paths, ok := trust["paths"].([]any)
		if !ok && trust["paths"] != nil {
			return nil, errors.New("[trust] paths must be an array of strings")
		}
		for _, item := range paths {
			value, ok := item.(string)
			if !ok {
				return nil, errors.New("[trust] paths must be an array of strings")
			}
			if samePath(value, root) {
				return original, nil
			}
		}
		paths = append(paths, root)
		values["trust"].(map[string]any)["paths"] = paths
		var rendered []string
		for _, item := range paths {
			rendered = append(rendered, tomlString(item.(string)))
		}
		text = upsert(text, "trust", "paths", "["+strings.Join(rendered, ", ")+"]", "\n")
		if _, _, err := profile.DecodeTOML([]byte(text)); err != nil {
			return nil, err
		}
		return []byte(text), nil
	})
}
