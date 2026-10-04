package ledger

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	OK      = "OK"
	MISSING = "MISSING"
	CORRUPT = "CORRUPT"
)

type Result struct {
	Status string         `json:"status"`
	Code   string         `json:"code"`
	Record map[string]any `json:"record"`
}

var secretValue = regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----|\bsk-[A-Za-z0-9_-]{20,}\b|\bgh[pousr]_[A-Za-z0-9]{20,}\b|\bAKIA[A-Z0-9]{16}\b|\bBearer\s+[A-Za-z0-9._~+/-]{12,}`)

var replaceFile = os.Rename

// PathForTask returns the Python SessionLedger path for taskID under root.
func PathForTask(root, taskID string) (string, error) {
	if taskID == "" {
		return "", fmt.Errorf("task_id must be a non-empty string")
	}
	digest := sha256.Sum256([]byte(taskID))
	return filepath.Join(root, "sessions", hex.EncodeToString(digest[:])+".json"), nil
}

// BuildSessionRecord mirrors Python build_session_record. Optional fields are
// supplied by their Python keyword names; nil values retain Python's defaults.
func BuildSessionRecord(taskID string, threadID *string, route, model, effort string, options map[string]any) map[string]any {
	record := map[string]any{
		"task_id": taskID, "live_thread": nil,
		"pending_call_baseline": options["pending_call_baseline"],
		"last_outcome":          options["last_outcome"],
	}
	if threadID == nil {
		return record
	}
	agent, ok := options["agent"]
	if !ok {
		agent = "codex"
	}
	thread := map[string]any{
		"task_id": taskID, "thread_id": *threadID, "route": route,
		"model": model, "effort": effort,
	}
	if agent != "codex" {
		thread["agent"] = agent
	}
	for _, key := range []string{"reason", "label", "task_objective", "acceptance_criteria"} {
		if value, exists := options[key]; exists && value != nil {
			thread[key] = value
		}
	}
	if phase, exists := options["phase"]; exists && phase != nil {
		thread["phase"] = phase
		if scope, exists := options["scope"]; exists && scope != nil {
			thread["scope"] = scope
		} else {
			thread["scope"] = []any{}
		}
	}
	grounded, exists := options["task_contract_grounded"]
	if !exists {
		grounded = false
	}
	thread["task_contract_grounded"] = grounded
	if checks, exists := options["profile_checks"]; exists && checks != nil {
		thread["profile_checks"] = checks
	}
	origin, hasOrigin := options["origin_baseline"].(map[string]any)
	post, hasPost := options["post_state"].(map[string]any)
	if hasOrigin && origin != nil && hasPost && post != nil {
		thread["origin_baseline"] = cloneMap(origin)
		thread["origin_scope"] = optionOrEmpty(options, "origin_scope")
		thread["post_state"] = cloneMap(post)
	}
	record["live_thread"] = thread
	return record
}

func optionOrEmpty(options map[string]any, key string) any {
	if value, exists := options[key]; exists && value != nil {
		return value
	}
	return []any{}
}

func cloneMap(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

// FindByThreadID returns the sole valid record for threadID. Invalid entries
// are ignored, and ambiguous matches return nil like Python SessionLedger.
func FindByThreadID(root, threadID string) map[string]any {
	if threadID == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(root, "sessions", "*.json"))
	if err != nil {
		return nil
	}
	var match map[string]any
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		record, ok := decodeRecord(raw, "")
		if !ok {
			continue
		}
		thread, ok := record["live_thread"].(map[string]any)
		if !ok || thread["thread_id"] != threadID {
			continue
		}
		if match != nil {
			return nil
		}
		match = record
	}
	return match
}

// Save validates and atomically writes a checksummed Python-compatible record.
func Save(root string, record map[string]any) Result {
	taskID, ok := record["task_id"].(string)
	if !ok || taskID == "" || !validRecord(record, taskID) {
		return Result{Status: "UNKNOWN", Code: "LEDGER_RECORD_INVALID"}
	}
	target, _ := PathForTask(root, taskID)
	payload, err := canonicalJSON(record)
	if err != nil {
		return Result{Status: "UNKNOWN", Code: "LEDGER_RECORD_INVALID"}
	}
	digest := sha256.Sum256(payload)
	envelope := map[string]any{"schema_version": 1, "checksum": hex.EncodeToString(digest[:]), "payload": record}
	encoded, err := canonicalJSON(envelope)
	if err != nil {
		return Result{Status: "UNKNOWN", Code: "LEDGER_RECORD_INVALID"}
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Result{Status: "UNKNOWN", Code: "LEDGER_WRITE_FAILED"}
	}
	temporary, err := os.CreateTemp(dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return Result{Status: "UNKNOWN", Code: "LEDGER_WRITE_FAILED"}
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if _, err = temporary.Write(encoded); err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = replaceFile(tempPath, target)
	}
	if err != nil {
		return Result{Status: "UNKNOWN", Code: "LEDGER_WRITE_FAILED"}
	}
	return Result{Status: OK, Code: "LEDGER_WRITTEN"}
}

// Load reads and validates one Python SessionLedger envelope without changing it.
func Load(path, taskID string) Result {
	if taskID == "" {
		return corrupt()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{Status: MISSING, Code: "LEDGER_RECORD_MISSING"}
		}
		return corrupt()
	}
	payload, ok := decodeRecord(raw, taskID)
	if !ok {
		return corrupt()
	}
	return Result{Status: OK, Code: "LEDGER_LOADED", Record: payload}
}

func decodeRecord(raw []byte, expectedTaskID string) (map[string]any, bool) {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil || !keys(envelope, "schema_version", "checksum", "payload") || number(envelope["schema_version"]) != 1 {
		return nil, false
	}
	payload, ok := envelope["payload"].(map[string]any)
	if !ok {
		return nil, false
	}
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return nil, false
	}
	checksum, ok := envelope["checksum"].(string)
	digest := sha256.Sum256(canonical)
	expected := hex.EncodeToString(digest[:])
	if !ok || len(checksum) != len(expected) || subtle.ConstantTimeCompare([]byte(checksum), []byte(expected)) != 1 || !validRecord(payload, expectedTaskID) {
		return nil, false
	}
	return payload, true
}

func canonicalJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte("\n")), nil
}

func corrupt() Result { return Result{Status: CORRUPT, Code: "LEDGER_CORRUPT"} }

func keys(value map[string]any, want ...string) bool {
	if len(value) != len(want) {
		return false
	}
	for _, key := range want {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func number(value any) float64 { n, _ := value.(float64); return n }

func validRecord(record map[string]any, expectedTaskID string) bool {
	if !keys(record, "task_id", "live_thread", "pending_call_baseline", "last_outcome") {
		return false
	}
	taskID, ok := record["task_id"].(string)
	if !ok || taskID == "" || (expectedTaskID != "" && taskID != expectedTaskID) {
		return false
	}
	if pending := record["pending_call_baseline"]; pending != nil {
		if _, ok := pending.(map[string]any); !ok {
			return false
		}
	}
	threadValue := record["live_thread"]
	if threadValue == nil {
		return !containsSecret(record)
	}
	thread, ok := threadValue.(map[string]any)
	if !ok {
		return false
	}
	allowed := map[string]bool{"task_id": true, "thread_id": true, "route": true, "model": true, "effort": true, "agent": true, "phase": true, "scope": true, "task_objective": true, "acceptance_criteria": true, "label": true, "task_contract_grounded": true, "reason": true, "profile_checks": true, "origin_baseline": true, "origin_scope": true, "post_state": true}
	for key := range thread {
		if !allowed[key] {
			return false
		}
	}
	for _, key := range []string{"task_id", "thread_id", "route", "model", "effort"} {
		value, ok := thread[key].(string)
		if !ok || value == "" {
			return false
		}
	}
	if thread["task_id"] != taskID {
		return false
	}
	if agent, exists := thread["agent"]; exists && agent != "codex" && agent != "opencode" && agent != "agy" {
		return false
	}
	if phase, exists := thread["phase"]; exists {
		if phase != "implementation" && phase != "operation" && phase != "plan" {
			return false
		}
		scope, ok := stringList(thread["scope"])
		if !ok || (phase == "implementation" && len(scope) == 0) {
			return false
		}
	}
	if grounded, exists := thread["task_contract_grounded"]; exists && grounded != true && grounded != false {
		return false
	}
	for _, key := range []string{"task_objective", "acceptance_criteria"} {
		if value, exists := thread[key]; exists {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	if thread["task_contract_grounded"] == true {
		if strings.TrimSpace(stringValue(thread["task_objective"])) == "" || strings.TrimSpace(stringValue(thread["acceptance_criteria"])) == "" {
			return false
		}
	}
	if label, exists := thread["label"]; exists {
		value, ok := label.(string)
		trimmed := strings.TrimSpace(value)
		printable := true
		for _, char := range trimmed {
			if !unicode.IsPrint(char) {
				printable = false
				break
			}
		}
		if !ok || trimmed == "" || utf8.RuneCountInString(trimmed) > 80 || !printable {
			return false
		}
	}
	if reason, exists := thread["reason"]; exists && !oneOf(reason, "new_task", "continue", "fix", "route_change", "reanchor", "repeated_failure", "user_override") {
		return false
	}
	if checks, exists := thread["profile_checks"]; exists {
		if _, ok := checks.([]any); !ok {
			return false
		}
	}
	_, hasBaseline := thread["origin_baseline"]
	_, hasScope := thread["origin_scope"]
	_, hasPost := thread["post_state"]
	if hasBaseline || hasScope || hasPost {
		if !(hasBaseline && hasScope && hasPost) {
			return false
		}
		origin, ok := thread["origin_baseline"].(map[string]any)
		if !ok || !validBaseline(origin) {
			return false
		}
		post, ok := thread["post_state"].(map[string]any)
		if !ok || !validBaseline(post) || origin["status"] != OK || post["status"] != OK {
			return false
		}
		scope, ok := stringList(thread["origin_scope"])
		originScope, originOK := stringList(origin["scope"])
		postScope, postOK := stringList(post["scope"])
		if !ok || !originOK || !postOK || !equalStrings(scope, originScope) || !equalStrings(scope, postScope) {
			return false
		}
	}
	return containsSecret(record) == false
}

func validBaseline(value map[string]any) bool {
	if value["status"] != OK {
		return false
	}
	head, ok := value["head"].(string)
	if !ok || head == "" {
		return false
	}
	if _, ok := stringList(value["scope"]); !ok {
		return false
	}
	if _, ok := value["manifest_enabled"].(bool); !ok {
		return false
	}
	if _, ok := value["status_entries"].([]any); !ok {
		return false
	}
	if _, ok := value["tracked_snapshots"].(map[string]any); !ok {
		return false
	}
	if _, ok := value["scoped_manifest"].([]any); !ok {
		return false
	}
	return true
}

func oneOf(value any, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringList(value any) ([]string, bool) {
	if stringsValue, ok := value.([]string); ok {
		out := append([]string(nil), stringsValue...)
		for _, item := range out {
			if item == "" {
				return nil, false
			}
		}
		return out, true
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i], ok = item.(string)
		if !ok || out[i] == "" {
			return nil, false
		}
	}
	return out, true
}

func stringValue(value any) string { result, _ := value.(string); return result }

func containsSecret(value any) bool { return secretAt(value, nil) }

func secretAt(value any, location []string) bool {
	switch item := value.(type) {
	case map[string]any:
		pathKeyed := equalStrings(location, []string{"pending_call_baseline", "tracked_snapshots"}) || equalStrings(location, []string{"live_thread", "origin_baseline", "tracked_snapshots"}) || equalStrings(location, []string{"live_thread", "post_state", "tracked_snapshots"})
		for key, child := range item {
			normalized := strings.NewReplacer("-", "_").Replace(strings.ToLower(key))
			if secretValue.MatchString(key) {
				return true
			}
			if !pathKeyed {
				for _, part := range []string{"api_key", "apikey", "credential", "password", "private_key", "secret", "token", "wallet"} {
					if strings.Contains(normalized, part) {
						return true
					}
				}
			}
			if secretAt(child, append(append([]string(nil), location...), key)) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if secretAt(child, location) {
				return true
			}
		}
	case string:
		return secretValue.MatchString(item)
	}
	return false
}
