package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

var codexTools = map[string]bool{
	"mcp__codex-main__codex": true, "mcp__codex-main__codex-reply": true,
	"mcp__plugin_purser_codex-main__codex": true, "mcp__plugin_purser_codex-main__codex-reply": true,
	"mcp__codex-main__codex_start": true, "mcp__codex-main__codex_result": true,
	"mcp__codex-main__codex_cancel": true, "mcp__plugin_purser_codex-main__codex_start": true,
	"mcp__plugin_purser_codex-main__codex_result": true, "mcp__plugin_purser_codex-main__codex_cancel": true,
	"mcp__codex-main__codex_apply": true, "mcp__plugin_purser_codex-main__codex_apply": true,
}

var runPreDecision = preDecision
var writePreBypass = writeBypass

// Pre dispatches Codex PreToolUse and recovers internal failures with Python's
// synchronous bypass / async-start deny contract. Normal policy remains Python-owned.
func Pre(event map[string]any, root string) (response map[string]any, failure error) {
	if event["hook_event_name"] != "PreToolUse" || !codexTools[stringValue(event["tool_name"])] {
		return nil, nil
	}
	metadata := map[string]any{}
	defer func() {
		if recovered := recover(); recovered != nil {
			failure = fmt.Errorf("panic: %v", recovered)
			response = preFailure(event, root, failure, metadata)
		}
	}()
	// Contract process tests require building the hook with -tags contracttest.
	internal := contractFault()
	tool := stringValue(event["tool_name"])
	if strings.HasSuffix(tool, "__codex_apply") {
		return preApply(event, root), nil
	}
	if strings.HasSuffix(tool, "__codex_result") || strings.HasSuffix(tool, "__codex_cancel") {
		if internal != nil {
			return syncBypass(event, root, internal, metadata), internal
		}
		return asyncPreFollowup(event, root), nil
	}
	if internal == nil {
		decision, err := runPreDecision(event, root, metadata)
		if err == nil {
			return decision, nil
		}
		internal = err
	}
	return preFailure(event, root, internal, metadata), internal
}

func preApply(event map[string]any, runtimeRoot string) map[string]any {
	input, ok := event["tool_input"].(map[string]any)
	if !ok {
		return denyPre("MALFORMED_TOOL_INPUT", "tool_input must be an object")
	}
	jobID := stringValue(input["jobId"])
	if jobID == "" {
		return denyPre("INVALID_JOB_ID", "jobId is required")
	}
	stateRoot := defaultPreStateRoot(runtimeRoot)
	_, subagent := event["agent_id"]
	if agent, valid := event["agent_id"].(string); valid && agent == "" {
		subagent = false
	}
	if input["acceptOnly"] == true {
		if subagent {
			return denyPre("SUBAGENT_ACCEPTANCE_DENIED", "only main Claude can accept a revision")
		}
		sessionID := stringValue(event["session_id"])
		revision := stringValue(input["revision"])
		current, valid := applyRevision(stateRoot, jobID, "")
		if sessionID == "" || !valid || revision == "" || revision != current {
			return denyPre("ACCEPTANCE_REVISION_UNAVAILABLE", "reviewed revision is unavailable")
		}
		updated := make(map[string]any, len(input)+2)
		for key, value := range input {
			updated[key] = value
		}
		updated["sessionId"] = sessionID
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "updatedInput": updated}}
	}
	if subagent {
		if _, valid := applyRevision(stateRoot, jobID, stringValue(event["session_id"])); !valid {
			return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse",
				"permissionDecision": "deny", "permissionDecisionReason": "main Claude has not accepted this revision"}}
		}
	}
	return map[string]any{}
}

func applyRevision(stateRoot, jobID, sessionID string) (string, bool) {
	jobPath := filepath.Join(stateRoot, "codex_exec_mcp", "jobs", jobID+".json")
	read := func() map[string]any {
		data, err := os.ReadFile(jobPath)
		if err != nil {
			return nil
		}
		var job map[string]any
		if json.Unmarshal(data, &job) != nil || job == nil {
			return nil
		}
		return job
	}
	job := read()
	if job == nil {
		return "", false
	}
	repo, _ := job["repoRoot"].(string)
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(root))
	unlock, err := lease.Lock(filepath.Join(filepath.Dir(filepath.Dir(jobPath)), "apply", hex.EncodeToString(sum[:])+".lock"))
	if err != nil {
		return "", false
	}
	defer func() { _ = unlock() }()
	job = read()
	if job == nil {
		return "", false
	}
	worktree, _ := job["worktreePath"].(string)
	snapshot, _ := job["snapshot"].(string)
	digest, err := ledger.JobChangedPathsSHA256(worktree, snapshot)
	if err != nil {
		return "", false
	}
	if sessionID == "" {
		return digest, true
	}
	acceptance, _ := job["acceptance"].(map[string]any)
	return digest, acceptance != nil && acceptance["sessionId"] == sessionID && acceptance["revision"] == digest
}

func asyncPreFollowup(event map[string]any, root string) map[string]any {
	stateRoot := defaultPreStateRoot(root)
	_, index, _, _, err := asyncOwned(stateRoot, event, "")
	if err != nil {
		var ownership asyncOwnershipError
		if errors.As(err, &ownership) && ownership.kind == "AsyncAdoptionRequired" {
			return denyPre("ASYNC_ADOPTION_REQUIRES_USER_CONSENT", "Ask the user before adopting this earlier session's job; after agreement call codex_result with adopt: true.")
		}
		return denyPre("UNKNOWN_ASYNC_JOB", "No pending job belongs to this session/project.")
	}
	if index["stage"] == "CONSUMED" {
		if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_cancel") {
			job := asyncJob(stateRoot, index)
			state := stringValue(job["state"])
			terminal := state == "SUCCEEDED" || state == "FAILED" || state == "CANCELLED" || state == "TIMED_OUT"
			_, hasWorktree := job["worktreePath"]
			worktree, _ := job["worktreePath"].(string)
			if job != nil && job["applyState"] != "applied" && (job["discarded"] == true || terminal && hasWorktree && worktree != "") {
				return map[string]any{}
			}
			return denyPre("ASYNC_ALREADY_CONSUMED", "This job has already been finalized.")
		}
		return map[string]any{}
	}
	if index["stage"] == "FINALIZING" {
		if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_result") {
			return map[string]any{}
		}
		return denyPre("ASYNC_FINALIZING", "This job is already being finalized.")
	}
	if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_result") {
		job := asyncJob(stateRoot, index)
		if job != nil && (job["state"] == "QUEUED" || job["state"] == "RUNNING") {
			status := "queued"
			if job["state"] == "RUNNING" {
				status = "still running"
			}
			return denyPre("ASYNC_STILL_RUNNING", status+"; wait for the completion signal")
		}
	}
	return map[string]any{}
}

func preFailure(event map[string]any, root string, internal error, metadata map[string]any) map[string]any {
	if strings.HasSuffix(stringValue(event["tool_name"]), "__codex_start") {
		if root != "" {
			_ = writeDiagnostic(root, event, internal)
		} else {
			_, _ = os.Stderr.Write(append(diagnosticJSON(diagnostic(event, internal)), '\n'))
		}
		return map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "deny",
			"permissionDecisionReason": "ASYNC_ENFORCEMENT_FAILED: Async start lacked a durable route/baseline; FULL review is required.",
		}}
	}
	return syncBypass(event, root, internal, metadata)
}

func syncBypass(event map[string]any, root string, cause error, metadata map[string]any) map[string]any {
	reason := safeError(cause)
	recorded := false
	if root != "" {
		if err := writePreBypass(root, event, reason, metadata); err == nil {
			recorded = true
		} else {
			reason += "; bypass persistence failed: " + safeError(err)
		}
	}
	if root != "" {
		if err := writeDiagnostic(root, event, cause); err != nil {
			_, _ = os.Stderr.Write(append(diagnosticJSON(diagnostic(event, cause)), '\n'))
		}
	} else {
		_, _ = os.Stderr.Write(append(diagnosticJSON(diagnostic(event, cause)), '\n'))
	}
	persistence := "The bypass record could not be persisted; this PreToolUse context is the only warning."
	if recorded {
		persistence = "The bypass was recorded for PostToolUse."
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "PreToolUse",
		"additionalContext": "Orchestrator routing was NOT enforced for this Codex call because of an internal error: " + reason + ". The call is allowed unmodified. " + persistence,
	}}
}

func writeBypass(root string, event map[string]any, reason string, metadata map[string]any) error {
	path, err := pendingPath(root, event)
	if err != nil {
		path = bypassPath(root, event)
	}
	record := map[string]any{
		"kind": "pretool_bypass", "routing_enforced": false, "baseline_captured": false,
		"reason": reason, "task_id": nil, "project_key": "unresolved", "phase": nil,
		"route": nil, "model": nil, "effort": nil, "scope": []any{}, "repo_root": nil,
		"prior_thread_id": nil,
	}
	for _, key := range []string{"task_id", "project_key", "phase", "route", "model", "effort", "scope", "repo_root", "prior_thread_id"} {
		if value, ok := metadata[key]; ok {
			record[key] = value
		}
	}
	data, err := json.Marshal(record)
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
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func WriteUndeliveredMarker(root string, event map[string]any) error {
	if event["hook_event_name"] != "WriteUndeliveredMarker" {
		return errors.New("invalid undelivered marker event")
	}
	tool := stringValue(event["tool_name"])
	if strings.HasSuffix(tool, "__codex_result") || strings.HasSuffix(tool, "__codex_cancel") {
		return nil
	}
	if !codexTools[tool] {
		return errors.New("tool is not eligible for an undelivered marker")
	}
	pending, err := pendingPath(root, event)
	if err != nil {
		return err
	}
	callKey := strings.TrimSuffix(filepath.Base(pending), ".json")
	markerPath := filepath.Join(root, "pending", "undelivered-"+callKey+".json")
	if strings.HasSuffix(tool, "__codex_start") {
		if err := cleanupUndeliveredAsyncStart(root, pending, markerPath); err != nil {
			return err
		}
		return nil
	}
	reason := stringValue(event["reason"])
	if len(reason) > 512 {
		reason = reason[:512]
	}
	data, err := json.Marshal(map[string]any{
		"schema": "purser.undelivered.v1", "call_key": callKey, "tool_name": tool,
		"reason": reason, "recorded_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	path := markerPath
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
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func cleanupUndeliveredAsyncStart(root, pendingPath, markerPath string) error {
	defer os.Remove(markerPath)
	data, err := os.ReadFile(pendingPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var pending map[string]any
		if err := json.Unmarshal(data, &pending); err != nil || pending == nil {
			return errors.New("async pending record is invalid")
		}
		if owner := stringValue(pending["lease_owner"]); owner != "" {
			project := stringValue(pending["project_key"])
			if project == "" {
				project = "unresolved"
			}
			_ = lease.New(filepath.Join(root, "projects", project)).Release(owner, float64(time.Now().UnixNano())/1e9)
		}
		_ = os.Remove(pendingPath)
	}
	return nil
}

func pendingPath(root string, event map[string]any) (string, error) {
	id, ok := event["tool_use_id"].(string)
	if !ok || id == "" {
		return "", errors.New("tool_use_id is missing")
	}
	session, _ := event["session_id"].(string)
	sum := sha256.Sum256([]byte(session + "\x00" + id))
	return filepath.Join(root, "pending", hex.EncodeToString(sum[:])+".json"), nil
}

func bypassPath(root string, event map[string]any) string {
	input, _ := canonicalJSON(event["tool_input"])
	session, _ := event["session_id"].(string)
	identity := session + "\x00" + stringValue(event["tool_name"]) + "\x00" + string(input)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(root, "pending", "bypass-"+hex.EncodeToString(sum[:])+".json")
}

func writeDiagnostic(root string, event map[string]any, cause error) error {
	path := filepath.Join(root, "diagnostics.jsonl")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	data := append(diagnosticJSON(diagnostic(event, cause)), '\n')
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func diagnostic(event map[string]any, cause error) map[string]any {
	return map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "phase": "PreToolUse",
		"hook_event_name": event["hook_event_name"], "tool_name": event["tool_name"],
		"tool_use_id": event["tool_use_id"], "error": safeError(cause)}
}

func safeError(err error) string {
	message := strings.TrimSpace(err.Error())
	var typed interface{ PostErrorKind() string }
	if errors.As(err, &typed) {
		if kind := typed.PostErrorKind(); kind != "" {
			if message == "" {
				return kind
			}
			return kind + ": " + message
		}
	}
	if message == "" {
		return "RuntimeError"
	}
	return fmt.Sprintf("RuntimeError: %s", message)
}

func stringValue(value any) string { result, _ := value.(string); return result }

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }

func diagnosticJSON(value any) []byte {
	compact := mustJSON(value)
	formatted := make([]byte, 0, len(compact)+8)
	quoted, escaped := false, false
	for _, char := range compact {
		if quoted {
			formatted = append(formatted, char)
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				quoted = false
			}
			continue
		}
		formatted = append(formatted, char)
		if char == '"' {
			quoted = true
		} else if char == ':' || char == ',' {
			formatted = append(formatted, ' ')
		}
	}
	return formatted
}

func canonicalJSON(value any) ([]byte, error) {
	var output strings.Builder
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(output.String(), "\n")), nil
}

var validThreadID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func projectKey(root *string) string {
	if root == nil {
		return "unresolved"
	}
	normalized := filepath.Clean(*root)
	if os.PathSeparator == '\\' {
		normalized = strings.ToLower(normalized)
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:16]
}

func threadIndexPath(runtimeRoot string, threadID any) (string, bool) {
	id, ok := threadID.(string)
	if !ok || !validThreadID.MatchString(id) {
		return "", false
	}
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(runtimeRoot, "threads", hex.EncodeToString(sum[:])+".json"), true
}

func resolvePath(value string) (string, error) {
	if strings.ContainsRune(value, 0) {
		return "", errors.New("invalid path")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func resolveCandidate(candidate string) (*string, string) {
	if strings.TrimSpace(candidate) == "" {
		return nil, "project root unresolved: hook input has no cwd"
	}
	directory, err := resolvePath(candidate)
	if err != nil {
		return nil, "project root unresolved: cwd is invalid"
	}
	root, err := ledger.GitTopLevel(directory)
	if err != nil {
		var failure *ledger.InspectionError
		errors.As(err, &failure)
		if failure != nil && failure.Code == "GIT_UNAVAILABLE" {
			return nil, "project root unresolved: git executable is unavailable"
		}
		if failure != nil && failure.Code == "GIT_FAILED" {
			return nil, "project root unresolved: cwd is not inside a git work tree"
		}
		return nil, "project root unresolved: git top-level lookup failed"
	}
	return &root, ""
}

func resolveProjectRoot(event map[string]any, explicitRoot *string) (*string, string) {
	if explicitRoot != nil {
		root, err := resolvePath(*explicitRoot)
		if err != nil {
			return nil, "project root unresolved: explicit root is invalid"
		}
		return &root, ""
	}
	var candidate string
	if input, ok := event["tool_input"].(map[string]any); ok {
		candidate, _ = input["cwd"].(string)
	}
	if strings.TrimSpace(candidate) == "" {
		candidate, _ = event["cwd"].(string)
	}
	return resolveCandidate(candidate)
}

func indexedReplyRoot(runtimeRoot string, threadID any) (*string, string) {
	path, ok := threadIndexPath(runtimeRoot, threadID)
	if !ok {
		return nil, "INVALID_THREAD_ID"
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ""
	}
	if err != nil || !utf8.Valid(raw) {
		return nil, "RECORDED_PROJECT_UNRESOLVED"
	}
	var entry map[string]any
	if json.Unmarshal(raw, &entry) != nil || entry == nil || entry["thread_id"] != threadID {
		return nil, "RECORDED_PROJECT_UNRESOLVED"
	}
	stored, ok := entry["project_root"].(string)
	if !ok || stored == "" || !filepath.IsAbs(stored) {
		return nil, "RECORDED_PROJECT_UNRESOLVED"
	}
	resolved, reason := resolveCandidate(stored)
	want, pathErr := resolvePath(stored)
	if resolved == nil || reason != "" || pathErr != nil || !samePath(*resolved, want) {
		return nil, "RECORDED_PROJECT_UNRESOLVED"
	}
	return resolved, ""
}

func samePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if os.PathSeparator == '\\' {
		return strings.EqualFold(left, right)
	}
	return left == right
}
