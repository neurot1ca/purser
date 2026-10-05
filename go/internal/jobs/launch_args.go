package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// OrderedObject preserves JSON object order where Python's dict iteration order
// is part of the generated argv. Callers that already have a map may pass it;
// those keys are flattened in sorted order for deterministic output.
type OrderedObject []ConfigEntry

type ConfigEntry struct {
	Key   string
	Value any
}

func (o *OrderedObject) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var token any
	if err := decoder.Decode(&token); err != nil {
		return err
	}
	if _, ok := token.(map[string]any); !ok {
		return fmt.Errorf("config must be an object")
	}
	// encoding/json's map decoder cannot retain order. Re-read object tokens
	// recursively to retain the source order.
	entries, err := decodeOrderedObject(data)
	if err != nil {
		return err
	}
	*o = entries
	return nil
}

func decodeOrderedObject(data []byte) ([]ConfigEntry, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var entries []ConfigEntry
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("config keys must be strings")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		value, err := decodeConfigValue(raw)
		if err != nil {
			return nil, err
		}
		entries = append(entries, ConfigEntry{Key: key, Value: value})
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return entries, nil
}

func decodeConfigValue(data []byte) (any, error) {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") {
		return decodeOrderedObject(data)
	}
	if strings.HasPrefix(trimmed, "[") {
		var rawValues []json.RawMessage
		if err := json.Unmarshal(data, &rawValues); err != nil {
			return nil, err
		}
		values := make([]any, 0, len(rawValues))
		for _, raw := range rawValues {
			value, err := decodeConfigValue(raw)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// LaunchArgs contains the Codex execution settings. Prompt text is deliberately
// absent: Codex reads it from stdin through the final "-" argument.
type LaunchArgs struct {
	BinaryOverride        string   `json:"binary_override"`
	Name                  string   `json:"name"` // "codex" or "codex-reply"
	Cwd                   string   `json:"cwd"`
	Model                 string   `json:"model"`
	Config                any      `json:"config"`
	Sandbox               string   `json:"sandbox"`
	ThreadID              string   `json:"thread_id"`
	OutputPath            string   `json:"output_path"`
	DeveloperInstructions *string  `json:"developer_instructions"`
	BaseInstructions      *string  `json:"base_instructions"`
	CompactPrompt         *string  `json:"compact_prompt"`
	ApprovalPolicy        *string  `json:"approval_policy"`
	Images                []string `json:"images"`
}

func (a *LaunchArgs) UnmarshalJSON(data []byte) error {
	type plain LaunchArgs
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if config, ok := fields["config"]; ok && string(config) != "null" {
		var ordered OrderedObject
		if err := json.Unmarshal(config, &ordered); err != nil {
			return err
		}
		decoded.Config = ordered
	}
	*a = LaunchArgs(decoded)
	return nil
}

// ResolveCodexPrefix mirrors Python's _binary_prefix, including bypassing
// cmd.exe for npm's Windows .cmd launcher.
func ResolveCodexPrefix(argument string) ([]string, error) {
	raw := argument
	if raw == "" {
		raw = os.Getenv("PURSER_CODEX_BIN")
	}
	var prefix []string
	if raw != "" {
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			switch value := parsed.(type) {
			case []any:
				if len(value) == 0 {
					return nil, fmt.Errorf("PURSER_CODEX_BIN JSON list must contain nonempty strings")
				}
				for _, item := range value {
					text, ok := item.(string)
					if !ok || text == "" {
						return nil, fmt.Errorf("PURSER_CODEX_BIN JSON list must contain nonempty strings")
					}
					prefix = append(prefix, text)
				}
			case string:
				if value == "" {
					return nil, fmt.Errorf("invalid Codex binary override")
				}
				prefix = []string{value}
			default:
				return nil, fmt.Errorf("invalid Codex binary override")
			}
		} else {
			prefix = []string{raw}
		}
	} else {
		resolved, err := exec.LookPath("codex")
		if err != nil {
			return nil, fmt.Errorf("Codex executable was not found")
		}
		prefix = []string{resolved}
	}
	executable := prefix[0]
	if runtime.GOOS == "windows" {
		ext := strings.ToLower(filepath.Ext(executable))
		if ext == "" {
			cmdPath := executable + ".cmd"
			if _, err := os.Stat(cmdPath); err == nil {
				executable = cmdPath
			}
		}
		ext = strings.ToLower(filepath.Ext(executable))
		if ext == ".cmd" || ext == ".bat" {
			entry := filepath.Join(filepath.Dir(executable), "node_modules", "@openai", "codex", "bin", "codex.js")
			if _, err := os.Stat(entry); err != nil {
				return nil, fmt.Errorf("Codex JS entry is missing beside launcher: %s", entry)
			}
			node, err := exec.LookPath("node")
			if err != nil {
				return nil, fmt.Errorf("node executable is required for the Codex JS entry")
			}
			return append([]string{node, entry}, prefix[1:]...), nil
		}
	}
	return append([]string{executable}, prefix[1:]...), nil
}

// FlattenConfig returns TOML key/value arguments in object iteration order.
func FlattenConfig(config any) ([]string, error) {
	var result []string
	if config == nil {
		return result, nil
	}
	if err := flattenConfig(&result, config, ""); err != nil {
		return nil, err
	}
	return result, nil
}

func flattenConfig(result *[]string, config any, prefix string) error {
	var entries []ConfigEntry
	switch value := config.(type) {
	case nil:
		return fmt.Errorf("config must be an object")
	case OrderedObject:
		entries = value
	case []ConfigEntry:
		entries = value
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			entries = append(entries, ConfigEntry{Key: key, Value: value[key]})
		}
	default:
		return fmt.Errorf("config must be an object")
	}
	for _, entry := range entries {
		key := entry.Key
		if key == "" || strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || strings.Contains(key, "..") {
			return fmt.Errorf("config keys must be nonempty strings")
		}
		name := key
		if prefix != "" {
			name = prefix + "." + key
		}
		if _, ok := entry.Value.(OrderedObject); ok {
			if err := flattenConfig(result, entry.Value, name); err != nil {
				return err
			}
		} else if nested, ok := entry.Value.([]ConfigEntry); ok {
			if err := flattenConfig(result, nested, name); err != nil {
				return err
			}
		} else if nested, ok := entry.Value.(map[string]any); ok {
			if err := flattenConfig(result, nested, name); err != nil {
				return err
			}
		} else {
			encoded, err := tomlValue(entry.Value)
			if err != nil {
				return err
			}
			*result = append(*result, name+"="+encoded)
		}
	}
	return nil
}

func tomlValue(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(v) + `"`, nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		if !strings.ContainsAny(string(v), ".eE") {
			return string(v), nil
		}
		f, err := v.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return "", fmt.Errorf("unsupported config value: float")
		}
		return pythonFloat(f), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", fmt.Errorf("unsupported config value: float")
		}
		return pythonFloat(v), nil
	case []any:
		items := make([]string, len(v))
		for i, item := range v {
			encoded, err := tomlValue(item)
			if err != nil {
				return "", err
			}
			items[i] = encoded
		}
		return "[" + strings.Join(items, ", ") + "]", nil
	default:
		return "", fmt.Errorf("unsupported config value: %T", value)
	}
}

func pythonFloat(value float64) string {
	absolute := math.Abs(value)
	if absolute == 0 || (absolute >= 1e-4 && absolute < 1e16) {
		formatted := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(formatted, ".") {
			formatted += ".0"
		}
		return formatted
	}
	formatted := strconv.FormatFloat(value, 'e', -1, 64)
	parts := strings.SplitN(formatted, "e", 2)
	exponent, _ := strconv.Atoi(parts[1])
	sign := "+"
	if exponent < 0 {
		sign = "-"
		exponent = -exponent
	}
	return parts[0] + "e" + sign + fmt.Sprintf("%02d", exponent)
}

// BuildCodexCommand constructs argv only; it never places prompt text there.
func BuildCodexCommand(args LaunchArgs) ([]string, error) {
	prefix, err := ResolveCodexPrefix(args.BinaryOverride)
	if err != nil {
		return nil, err
	}
	configArgs, err := FlattenConfig(args.Config)
	if err != nil {
		return nil, err
	}
	for _, option := range []struct {
		key   string
		value *string
	}{
		{"developer_instructions", args.DeveloperInstructions},
		{"base_instructions", args.BaseInstructions},
		{"compact_prompt", args.CompactPrompt},
		{"approval_policy", args.ApprovalPolicy},
	} {
		if option.value != nil {
			if option.key == "approval_policy" && *option.value != "on-request" && *option.value != "never" {
				return nil, fmt.Errorf("invalid approval-policy")
			}
			configArgs = append(configArgs, option.key+"="+mustTOMLString(*option.value))
		}
	}
	command := append([]string{}, prefix...)
	command = append(command, "exec")
	if args.Name == "codex-reply" {
		command = append(command, "resume")
	}
	command = append(command, "--json", "--skip-git-repo-check")
	if args.Model != "" {
		command = append(command, "-m", args.Model)
	}
	for _, value := range configArgs {
		command = append(command, "-c", value)
	}
	if args.Name == "codex" {
		if args.Sandbox != "" {
			command = append(command, "-s", args.Sandbox)
		}
		command = append(command, "-C", args.Cwd)
	}
	for _, image := range args.Images {
		command = append(command, "--image", image)
	}
	command = append(command, "-o", filepath.Clean(args.OutputPath))
	if args.Name == "codex-reply" {
		command = append(command, args.ThreadID)
	}
	return append(command, "-"), nil
}

func mustTOMLString(value string) string {
	encoded, _ := tomlValue(value)
	return encoded
}
