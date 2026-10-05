package jobs

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const outputTokenLimitPrefix = "tool_output_token_limit="

// StateDir returns the thread-record directory used by the Python bridge.
func StateDir() string {
	if override := os.Getenv("PURSER_CODEX_SHIM_STATE"); override != "" {
		return absolute(resolveHome(override))
	}
	root := os.Getenv("PURSER_STATE_DIR")
	if root == "" {
		if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" {
			root = filepath.Join(resolveHome(config), "purser")
		} else if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, ".claude", "purser")
		}
	} else {
		root = resolveHome(root)
	}
	return filepath.Join(absolute(root), "codex_exec_mcp", "threads")
}

func absolute(path string) string {
	if path == "" {
		return ""
	}
	if result, err := filepath.Abs(path); err == nil {
		return result
	}
	return path
}

func resolveHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// RecordPath hashes the UTF-8 thread ID exactly as the Python bridge does.
func RecordPath(stateDir, threadID string) string {
	sum := sha256.Sum256([]byte(threadID))
	return filepath.Join(stateDir, hex.EncodeToString(sum[:])+".json")
}

// SaveRecord durably replaces a thread record while retaining all supplied fields.
func SaveRecord(stateDir string, record map[string]any) error {
	threadID, ok := record["thread_id"].(string)
	if !ok {
		return errors.New("thread_id must be a string")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := filepath.Join(stateDir, ".thread-"+hex.EncodeToString(nonce[:])+".json")
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	data, err := marshalPythonJSON(record)
	if err != nil {
		_ = file.Close()
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, RecordPath(stateDir, threadID))
}

func marshalPythonJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writePythonJSON(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writePythonJSON(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ordered := make([]string, 0, len(keys))
		for _, key := range []string{"thread_id", "cwd", "model", "config_args", "sandbox", "agent", "worktreePath", "snapshot", "baselineSnapshot", "head", "repoRoot"} {
			if _, ok := value[key]; ok {
				ordered = append(ordered, key)
			}
		}
		known := make(map[string]bool, len(ordered))
		for _, key := range ordered {
			known[key] = true
		}
		for _, key := range keys {
			if !known[key] {
				ordered = append(ordered, key)
			}
		}
		out.WriteByte('{')
		for index, key := range ordered {
			if index > 0 {
				out.WriteString(", ")
			}
			encodedKey, err := marshalScalar(key)
			if err != nil {
				return err
			}
			out.Write(encodedKey)
			out.WriteString(": ")
			if err := writePythonJSON(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for index, item := range value {
			if index > 0 {
				out.WriteString(", ")
			}
			if err := writePythonJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	default:
		encoded, err := marshalScalar(value)
		if err != nil {
			return err
		}
		out.Write(encoded)
	}
	return nil
}

func marshalScalar(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode thread record: %w", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// LoadRecord returns nil for missing, corrupt, or invalid records, as the bridge does.
func LoadRecord(stateDir, threadID string) map[string]any {
	data, err := os.ReadFile(RecordPath(stateDir, threadID))
	if err != nil {
		return nil
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil || record == nil {
		return nil
	}
	if record["thread_id"] != threadID {
		return nil
	}
	if _, ok := record["cwd"].(string); !ok {
		return nil
	}
	args, ok := record["config_args"].([]any)
	if !ok {
		return nil
	}
	for _, arg := range args {
		if _, ok := arg.(string); !ok {
			return nil
		}
	}
	return record
}

// ReplyConfig applies the configuration reuse rules for a direct codex-reply.
func ReplyConfig(record map[string]any) map[string]any {
	if record == nil {
		return nil
	}
	result := make(map[string]any, len(record))
	for key, value := range record {
		result[key] = value
	}
	if worktree, ok := record["worktreePath"].(string); ok && worktree != "" {
		result["cwd"] = worktree
	}
	if args, ok := record["config_args"].([]any); ok {
		filtered := make([]any, 0, len(args)+1)
		sandbox, _ := record["sandbox"].(string)
		for _, arg := range args {
			text, _ := arg.(string)
			if strings.HasPrefix(text, outputTokenLimitPrefix) || (sandbox == "read-only" && strings.HasPrefix(text, "sandbox_mode=")) {
				continue
			}
			filtered = append(filtered, arg)
		}
		if sandbox == "read-only" {
			filtered = append(filtered, `sandbox_mode="read-only"`)
		}
		result["config_args"] = filtered
	}
	return result
}

// ReplyMetadata applies the durable reply metadata aliases in bridge precedence order.
func ReplyMetadata(args map[string]any) map[string]any {
	result := make(map[string]any, 4)
	for _, field := range []struct{ stored, supplied string }{
		{"model", "jobModel"}, {"effort", "jobEffort"}, {"label", "jobLabel"}, {"phase", "jobPhase"},
	} {
		value, exists := args[field.supplied]
		if !exists {
			value = args[field.stored]
		}
		if value != nil {
			result[field.stored] = value
		}
	}
	return result
}
