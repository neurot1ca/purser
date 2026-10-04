package enforce

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"purser-hook/internal/contextio"
)

const observationSchemaVersion = 1

// observationAggregateDefaults mirrors telemetry._new_aggregate's routing
// fields. Existing values are overlaid after these defaults by the task fold.
func observationAggregateDefaults(taskID, projectKey any) map[string]any {
	usage := map[string]any{
		"codex_call_count": 0, "codex_duration_ms": nil, "codex_usage_known": false,
		"gemini_duration_ms": nil, "gemini_usage_known": false, "deepseek_usage_known": false,
		"claude_5h_start_pct": nil, "claude_5h_end_pct": nil, "claude_5h_delta_pct": nil,
		"claude_7d_start_pct": nil, "claude_7d_end_pct": nil, "claude_context_used_percentage": nil,
		"claude_session_estimated_cost_usd": nil, "claude_usage_known": false,
		"claude_rate_limit_delta_scope": "account_window_observation",
		"claude_task_usage_source":      nil, "claude_task_usage_known": false, "claude_task_usage_complete": false,
		"claude_task_input_tokens": nil, "claude_task_output_tokens": nil,
		"claude_task_cache_creation_input_tokens": nil, "claude_task_cache_read_input_tokens": nil,
		"claude_task_total_observed_tokens": nil, "claude_task_request_count": nil,
		"claude_task_usage_boundary": nil, "claude_task_usage_dedup": nil,
		"claude_task_usage_unknown_reason": nil, "claude_task_subagent_usage_included": false,
	}
	for _, provider := range []string{"codex", "gemini", "deepseek"} {
		for _, field := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cached_tokens", "cache_read_tokens", "cache_write_tokens", "thinking_tokens"} {
			usage[provider+"_"+field] = nil
		}
	}
	for _, prefix := range []string{"_claude_5h_start_", "_claude_5h_end_"} {
		for _, suffix := range []string{"session_id", "captured_at", "resets_at"} {
			usage[prefix+suffix] = nil
		}
	}
	return map[string]any{
		"task_id": taskID, "project_key": projectKey, "repo": nil, "branch": nil,
		"status": "UNKNOWN_OR_IN_PROGRESS", "started_at": nil, "ended_at": nil, "duration_ms": nil,
		"phase": nil, "task_type": "unknown", "task_type_source": "unknown", "has_code_map": nil,
		"selected_agent": nil, "selected_provider": nil, "selected_model": nil,
		"served_agent": nil, "served_provider": nil, "served_model": nil, "served_route": nil, "served_effort": nil,
		"claude_decision":   nil,
		"claude_acceptance": map[string]any{"outcome": "unknown", "send_backs": 0, "applied_as_is": false, "sources": []any{}},
		"route":             nil, "model": nil, "effort": nil, "validation": nil, "thread_id": nil,
		"fresh_threads": 0, "reply_calls": 0, "routing_enforced": nil,
		"reviewer1_provider": nil, "reviewer1_model_requested": nil, "served_model_attestation": "UNAVAILABLE",
		"reviewer1_result": nil, "reviewer1_initial_finding_count": nil, "reviewer1_skipped_reason": nil,
		"claude_review_mode": nil, "changed_paths_count": nil, "routing_protocol_violations": 0,
		"thread_continuity_violations": 0, "concurrent_writer": false,
		"policy_fingerprint": nil,
		"policy_hashes":      map[string]any{"skill_sha256": nil, "config_sha256": nil, "hook_sha256": nil},
		"usage":              usage, "context_io": nil,
	}
}

// foldObservationAggregate applies the routing-related Python task fold in
// event order and returns whether terminal Claude decisions conflict.
func foldObservationAggregate(existing map[string]any, taskID, projectKey any, events []map[string]any, incremental bool) (map[string]any, bool) {
	defaults := observationAggregateDefaults(taskID, projectKey)
	aggregate := defaults
	if hashes, hasHashes := existing["policy_hashes"]; hasHashes {
		if fingerprint, hasFingerprint := existing["policy_fingerprint"]; hasFingerprint {
			aggregate["policy_hashes"], aggregate["policy_fingerprint"] = hashes, fingerprint
		}
	}
	usage := defaults["usage"].(map[string]any)
	if incremental {
		for key, value := range existing {
			aggregate[key] = value
		}
		if saved, ok := existing["usage"].(map[string]any); ok {
			for key, value := range saved {
				usage[key] = value
			}
		}
	}
	aggregate["usage"] = usage
	ordered := append([]map[string]any(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := fmt.Sprint(ordered[i]["ts_utc"]), fmt.Sprint(ordered[j]["ts_utc"])
		if left != right {
			return left < right
		}
		return fmt.Sprint(ordered[i]["event_id"]) < fmt.Sprint(ordered[j]["event_id"])
	})
	conflict := false
	for _, event := range ordered {
		payload, _ := event["payload"].(map[string]any)
		for _, field := range []string{"repo", "branch"} {
			if aggregate[field] == nil && event[field] != nil {
				aggregate[field] = event[field]
			}
		}
		switch event["event_type"] {
		case "TASK_ROUTE_SELECTED":
			for _, field := range []string{"phase", "route", "model", "effort", "routing_enforced"} {
				if value, ok := payload[field]; ok {
					aggregate[field] = value
				}
			}
			if value, ok := payload["task_type"]; ok {
				aggregate["task_type"], aggregate["task_type_source"] = value, "route"
			}
			for source, target := range map[string]string{"agent": "selected_agent", "provider": "selected_provider", "model": "selected_model"} {
				if value, ok := payload[source]; ok {
					aggregate[target] = value
				}
			}
		case "IMPLEMENTER_STARTED":
			aggregate["status"] = "IN_PROGRESS"
			if aggregate["started_at"] == nil || aggregate["started_at"] == "" {
				aggregate["started_at"] = event["ts_utc"]
			}
			if payload["fresh_thread"] == true {
				aggregate["fresh_threads"] = numberOrZero(aggregate["fresh_threads"]) + 1
			}
			if payload["reply_call"] == true {
				aggregate["reply_calls"] = numberOrZero(aggregate["reply_calls"]) + 1
			}
			usage["codex_call_count"] = numberOrZero(usage["codex_call_count"]) + 1
			foldClaudeUsage(usage, payload, "start")
		case "IMPLEMENTER_FINISHED", "IMPLEMENTER_FAILED":
			aggregate["ended_at"] = event["ts_utc"]
			aggregate["duration_ms"] = addObservationNumber(aggregate["duration_ms"], payload["duration_ms"])
			usage["codex_duration_ms"] = addObservationNumber(usage["codex_duration_ms"], payload["duration_ms"])
			for _, field := range []string{"changed_paths_count", "thread_id", "claude_review_mode", "routing_enforced", "served_agent", "served_provider", "served_model", "served_route", "served_effort"} {
				if value, ok := payload[field]; ok {
					aggregate[field] = value
				}
			}
			if value, ok := payload["has_code_map"]; ok {
				aggregate["has_code_map"] = value
			}
			if aggregate["validation"] == nil && payload["validation"] != nil {
				aggregate["validation"] = payload["validation"]
			}
			known := payload["codex_usage_known"] == true
			if numberOrZero(usage["codex_call_count"]) <= 1 {
				usage["codex_usage_known"] = known
			} else {
				usage["codex_usage_known"] = usage["codex_usage_known"] == true && known
			}
			for _, field := range observationTokenFields {
				key := "codex_" + field
				if value, ok := payload[key]; ok {
					usage[key] = addObservationNumber(usage[key], value)
				}
			}
			foldClaudeUsage(usage, payload, "end")
		case "REVIEWER1_STARTED", "REVIEWER1_FINISHED", "REVIEWER1_FAILED":
			if event["event_type"] == "REVIEWER1_STARTED" {
				aggregate["reviewer1_result"], aggregate["reviewer1_initial_finding_count"], aggregate["reviewer1_skipped_reason"] = nil, nil, nil
			} else {
				aggregate["reviewer1_skipped_reason"] = nil
			}
			for _, field := range []string{"reviewer1_provider", "reviewer1_model_requested", "served_model_attestation", "reviewer1_result", "reviewer1_initial_finding_count"} {
				if value, ok := payload[field]; ok {
					aggregate[field] = value
				}
			}
			if event["event_type"] != "REVIEWER1_STARTED" {
				usage["gemini_duration_ms"] = addObservationNumber(usage["gemini_duration_ms"], payload["duration_ms"])
				if payload["gemini_usage_known"] == true {
					usage["gemini_usage_known"] = true
				}
				for _, field := range observationTokenFields {
					key := "gemini_" + field
					if value, ok := payload[key]; ok {
						usage[key] = addObservationNumber(usage[key], value)
					}
				}
			}
		case "REVIEWER1_SKIPPED":
			aggregate["reviewer1_provider"], aggregate["reviewer1_result"], aggregate["reviewer1_initial_finding_count"] = nil, nil, nil
			aggregate["reviewer1_skipped_reason"] = payload["reviewer1_skipped_reason"]
			aggregate["reviewer1_model_requested"] = payload["reviewer1_model_requested"]
		case "ROUTING_PROTOCOL_VIOLATION":
			aggregate["routing_protocol_violations"] = numberOrZero(aggregate["routing_protocol_violations"]) + 1
		case "THREAD_CONTINUITY_VIOLATION":
			aggregate["thread_continuity_violations"] = numberOrZero(aggregate["thread_continuity_violations"]) + 1
		case "UNKNOWN_CONCURRENT_WRITER":
			aggregate["concurrent_writer"] = true
		case "CLAUDE_DECISION_RECORDED":
			acceptance, _ := aggregate["claude_acceptance"].(map[string]any)
			source, _ := payload["source"].(map[string]any)
			sources, _ := acceptance["sources"].([]any)
			duplicate := false
			for _, raw := range sources {
				if item, ok := raw.(map[string]any); ok && item["kind"] == source["kind"] && item["job_id"] == source["job_id"] {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			terminalsBefore := map[string]bool{}
			for _, raw := range sources {
				if item, ok := raw.(map[string]any); ok {
					decision := fmt.Sprint(item["decision"])
					if decision == "accepted" || decision == "rejected" {
						terminalsBefore[decision] = true
					}
				}
			}
			decision := payload["decision"]
			entry := map[string]any{"kind": source["kind"], "job_id": source["job_id"], "decision": decision, "task_type": payload["task_type"], "ts_utc": event["ts_utc"]}
			sources = append(sources, entry)
			acceptance["sources"] = sources
			aggregate["claude_decision"] = map[string]any{"decision": decision, "task_type": payload["task_type"], "source": source, "ts_utc": event["ts_utc"]}
			if aggregate["task_type"] == "unknown" && payload["task_type"] != "unknown" {
				aggregate["task_type"], aggregate["task_type_source"] = payload["task_type"], "decision"
			}
			if decision == "send_back" {
				acceptance["send_backs"] = numberOrZero(acceptance["send_backs"]) + 1
			}
			terminals := map[string]bool{}
			for _, raw := range sources {
				if item, ok := raw.(map[string]any); ok {
					d := fmt.Sprint(item["decision"])
					if d == "accepted" || d == "rejected" {
						terminals[d] = true
					}
				}
			}
			outcome := "unknown"
			if len(terminals) == 1 {
				for d := range terminals {
					outcome = d
				}
			}
			acceptance["outcome"] = outcome
			acceptance["applied_as_is"] = outcome == "accepted" && numberOrZero(acceptance["send_backs"]) == 0
			conflict = conflict || len(terminals) > 1 && len(terminalsBefore) <= 1
		case "CLAUDE_TASK_USAGE_RECORDED":
			foldClaudeTaskUsage(usage, payload)
		case "CONTEXT_IO_TURN_RECORDED":
			copy := map[string]any{}
			for key, value := range payload {
				copy[key] = value
			}
			aggregate["context_io"] = copy
		}
	}
	return aggregate, conflict
}

var observationTokenFields = strings.Fields("input_tokens cached_input_tokens output_tokens reasoning_output_tokens total_tokens prompt_tokens completion_tokens cached_tokens cache_read_tokens cache_write_tokens thinking_tokens")

func addObservationNumber(current, value any) any {
	var n float64
	switch x := value.(type) {
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case float64:
		n = x
	default:
		return current
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return current
	}
	if current == nil {
		return value
	}
	switch x := current.(type) {
	case int:
		return float64(x) + n
	case int64:
		return float64(x) + n
	case float64:
		return x + n
	}
	return current
}

func foldClaudeUsage(usage, payload map[string]any, endpoint string) {
	if payload["claude_usage_known"] == true {
		usage["claude_usage_known"] = true
	}
	if payload["claude_rate_limit_delta_scope"] == "account_window_observation" {
		usage["claude_rate_limit_delta_scope"] = "account_window_observation"
	}
	for _, field := range []string{"claude_context_used_percentage", "claude_session_estimated_cost_usd"} {
		if n := observationClaudeNumber(payload[field]); n != nil {
			usage[field] = n
		}
	}
	endpointField := "claude_5h_" + endpoint + "_pct"
	endpointValue := observationClaudeNumber(payload[endpointField])
	if endpoint == "start" {
		if endpointValue != nil && usage["claude_5h_start_pct"] == nil {
			usage["claude_5h_start_pct"] = endpointValue
			setClaudeUsageEndpoint(usage, payload, "start")
		}
	} else if endpointValue != nil {
		usage["claude_5h_end_pct"] = endpointValue
		setClaudeUsageEndpoint(usage, payload, "end")
	}
	if n := observationClaudeNumber(payload["claude_7d_"+endpoint+"_pct"]); n != nil {
		usage["claude_7d_"+endpoint+"_pct"] = n
	}
	start, end := observationClaudeNumber(usage["claude_5h_start_pct"]), observationClaudeNumber(usage["claude_5h_end_pct"])
	if start == nil || end == nil || usage["_claude_5h_start_session_id"] == nil || usage["_claude_5h_start_session_id"] != usage["_claude_5h_end_session_id"] || claudeResetCrossed(usage) {
		usage["claude_5h_delta_pct"] = nil
		return
	}
	if startAt, ok1 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_start_captured_at"])); ok1 {
		if endAt, ok2 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_end_captured_at"])); ok2 && endAt.Before(startAt) {
			usage["claude_5h_delta_pct"] = nil
			return
		}
	}
	usage["claude_5h_delta_pct"] = end.(float64) - start.(float64)
}

func setClaudeUsageEndpoint(usage, payload map[string]any, endpoint string) {
	for suffix, field := range map[string]string{"session_id": "claude_usage_session_id", "captured_at": "claude_usage_captured_at", "resets_at": "claude_5h_resets_at"} {
		usage["_claude_5h_"+endpoint+"_"+suffix] = payload[field]
	}
}

func observationClaudeNumber(value any) any {
	switch n := value.(type) {
	case int:
		if n >= 0 {
			return float64(n)
		}
	case int64:
		if n >= 0 {
			return float64(n)
		}
	case float64:
		if n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n
		}
	}
	return nil
}

func claudeResetCrossed(usage map[string]any) bool {
	startReset, ok1 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_start_resets_at"]))
	endReset, ok2 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_end_resets_at"]))
	if ok1 && ok2 && !startReset.Equal(endReset) {
		return true
	}
	startAt, ok3 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_start_captured_at"]))
	endAt, ok4 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_end_captured_at"]))
	return ok1 && ok2 && ok3 && ok4 && !startAt.After(endAt) && !startReset.Before(startAt) && !startReset.After(endAt)
}

func foldClaudeTaskUsage(usage, payload map[string]any) {
	terminal := fields("OVERLAPPING_TASK_BOUNDARY CONTRADICTORY_USAGE_RECORD TRANSCRIPT_IDENTITY_MISMATCH TRANSCRIPT_ROTATED_OR_TRUNCATED TRANSCRIPT_UNRESOLVED_TAIL BOUNDS_EXCEEDED INVALID_USAGE_RECORD STALE_ACTIVE_TASK ANCHOR_NOT_FOUND TRANSCRIPT_UNAVAILABLE SESSION_MISMATCH WRONG_EXTENSION TRANSCRIPT_PATH_ESCAPE NO_USAGE_RECORDS ACTIVE_TASK_STATE_CONTENTION")
	currentReason := asString(usage["claude_task_usage_unknown_reason"])
	if terminal[currentReason] {
		return
	}
	newReason := payload["claude_task_usage_unknown_reason"]
	known := payload["claude_task_usage_known"] == true && !terminal[asString(newReason)]
	fieldsToClear := []string{"claude_task_input_tokens", "claude_task_output_tokens", "claude_task_cache_creation_input_tokens", "claude_task_cache_read_input_tokens", "claude_task_total_observed_tokens", "claude_task_request_count"}
	if terminal[asString(newReason)] || !known {
		usage["claude_task_usage_known"], usage["claude_task_usage_complete"] = false, false
		usage["claude_task_usage_unknown_reason"] = newReason
		usage["claude_task_usage_source"], usage["claude_task_usage_boundary"], usage["claude_task_usage_dedup"] = nil, nil, nil
		for _, field := range fieldsToClear {
			usage[field] = nil
		}
		usage["claude_task_subagent_usage_included"] = false
		return
	}
	usage["claude_task_usage_known"] = true
	usage["claude_task_usage_complete"] = payload["claude_task_usage_complete"] == true
	usage["claude_task_usage_unknown_reason"] = nil
	for _, field := range []string{"claude_task_usage_source", "claude_task_usage_boundary", "claude_task_usage_dedup"} {
		usage[field] = payload[field]
	}
	usage["claude_task_subagent_usage_included"] = payload["claude_task_subagent_usage_included"] == true
	for _, field := range fieldsToClear {
		if n, ok := payload[field].(float64); ok && n >= 0 && math.Trunc(n) == n {
			usage[field] = n
		}
	}
}

func numberOrZero(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func fields(names string) map[string]bool {
	out := map[string]bool{}
	for _, name := range strings.Fields(names) {
		out[name] = true
	}
	return out
}

var (
	callObservationFields  = fields("phase route model model_source effort routing_enforced fresh_thread reply_call reason reason_source previous_route new_route eta_minutes worker_prompt_chars task_type suspected_route_mismatch agent provider")
	startObservationFields = fields("implementer claude_usage_session_id claude_usage_captured_at claude_5h_start_pct claude_5h_end_pct claude_5h_resets_at claude_7d_start_pct claude_7d_end_pct claude_7d_resets_at claude_context_used_percentage claude_context_window_size claude_context_total_tokens claude_context_input_tokens claude_context_output_tokens claude_cache_creation_input_tokens claude_cache_read_input_tokens claude_session_estimated_cost_usd claude_session_total_duration_ms claude_usage_known claude_rate_limit_delta_scope")
	observationFields      = map[string]map[string]bool{"TASK_ROUTE_SELECTED": callObservationFields, "IMPLEMENTER_STARTED": mergeFields(callObservationFields, startObservationFields), "SCOPE_CONFLICT_OUTCOME": fields("action job_id queue_wait_ms"), "ROUTING_PROTOCOL_VIOLATION": fields("violation_code"), "THREAD_CONTINUITY_VIOLATION": fields("violation_code")}
	observationID          = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	observationUUID        = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	unsafeObservationID    = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	observationProjectKey  = regexp.MustCompile(`^(?:[0-9a-f]{16}|unresolved)$`)
	modelValue             = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)
)

// persistObservation mirrors telemetry.emit's best-effort append contract.
// On lock contention the complete event is atomically spooled and is not
// written to the task index until a later recovery pass merges it.
func persistObservation(root string, event map[string]any) (string, error) {
	eventID, ok := event["event_id"].(string)
	if !ok || !observationID.MatchString(eventID) {
		return "TELEMETRY_EMIT_FAILED", fmt.Errorf("invalid telemetry event id")
	}
	encoded, err := canonicalJSON(event)
	if err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	events := filepath.Join(root, "events")
	if err = os.MkdirAll(events, 0700); err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	lockPath := filepath.Join(events, ".append.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	defer lock.Close()
	if info, statErr := lock.Stat(); statErr != nil {
		return "TELEMETRY_EMIT_FAILED", statErr
	} else if info.Size() == 0 {
		if _, err = lock.WriteAt([]byte("0"), 0); err != nil {
			return "TELEMETRY_EMIT_FAILED", err
		}
	}
	if err = contextio.TryLock(lock); err != nil {
		spool := filepath.Join(events, "spool", eventID+".json")
		if err = atomicObservationJSON(spool, encoded); err != nil {
			return "TELEMETRY_EMIT_FAILED", err
		}
		return "", nil
	}
	defer contextio.Unlock(lock)
	if err = mergeObservationSpool(root); err != nil {
		return "TELEMETRY_SPOOL_MERGE_FAILED", err
	}
	month := ""
	if timestamp, valid := event["ts_utc"].(string); valid && len(timestamp) >= 7 {
		month = timestamp[:7]
	}
	session, _ := event["session_id"].(string)
	if session == "" {
		session = "unknown"
	}
	path := filepath.Join(events, month, "session_"+session+".jsonl")
	if err = appendObservationRecord(path, encoded); err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	if task, ok := event["task_id"].(string); ok {
		if err = appendObservationRecord(filepath.Join(events, "by_task", task+".jsonl"), encoded); err != nil {
			return "TELEMETRY_EMIT_FAILED", err
		}
	}
	return "", nil
}

const observationSpoolMergeLimit = 16

// mergeObservationSpool mirrors telemetry._merge_spool. The marker is created
// before writes, as in Python; on recovery each destination is checked by
// event ID so a crash between either append resumes without duplicates.
func mergeObservationSpool(root string) error {
	spool := filepath.Join(root, "events", "spool")
	entries, err := os.ReadDir(spool)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	merged := filepath.Join(spool, ".merged")
	if err = os.MkdirAll(merged, 0700); err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if count == observationSpoolMergeLimit {
			break
		}
		count++
		path := filepath.Join(spool, entry.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			recordObservationHealth(root, "DEGRADED", "MALFORMED_SPOOL_EVENT")
			_ = os.Remove(path)
			continue
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			recordObservationHealth(root, "DEGRADED", "MALFORMED_SPOOL_EVENT")
			_ = os.Remove(path)
			continue
		}
		if !validObservationSpoolEvent(event) {
			recordObservationHealth(root, "DEGRADED", "MALFORMED_SPOOL_EVENT")
			_ = os.Remove(path)
			continue
		}
		eventID := event["event_id"].(string)
		marker := filepath.Join(merged, eventID)
		if _, statErr := os.Stat(marker); os.IsNotExist(statErr) {
			file, createErr := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if createErr != nil && !os.IsExist(createErr) {
				return createErr
			}
			if createErr == nil {
				if closeErr := file.Close(); closeErr != nil {
					return closeErr
				}
			}
		} else if statErr != nil {
			return statErr
		}
		line := append(append([]byte(nil), raw...), '\n')
		ts := event["ts_utc"].(string)
		session, _ := event["session_id"].(string)
		if session == "" {
			session = "unknown"
		}
		canonical := filepath.Join(root, "events", ts[:7], "session_"+session+".jsonl")
		if err = appendObservationOnce(canonical, line, eventID); err != nil {
			return err
		}
		if task, ok := event["task_id"].(string); ok {
			if err = appendObservationOnce(filepath.Join(root, "events", "by_task", task+".jsonl"), line, eventID); err != nil {
				return err
			}
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func validObservationSpoolEvent(event map[string]any) bool {
	keys := []string{"schema_version", "event_id", "ts_utc", "event_type", "task_id", "project_key", "session_id", "repo", "branch", "agent_id", "agent_type", "payload"}
	if len(event) != len(keys) || !observationUUID.MatchString(asString(event["event_id"])) || event["schema_version"] != float64(observationSchemaVersion) {
		return false
	}
	for _, key := range keys {
		if _, ok := event[key]; !ok {
			return false
		}
	}
	ts, ok := event["ts_utc"].(string)
	if !ok || len(ts) > 40 {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return false
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return false
	}
	task := event["task_id"]
	envelope := map[string]any{}
	for _, name := range []string{"project_key", "session_id", "repo", "branch", "agent_id", "agent_type"} {
		envelope[name] = event[name]
	}
	payload, ok := event["payload"].(map[string]any)
	if !ok {
		return false
	}
	built, err := buildObservation(asString(event["event_type"]), envelope, payload, task)
	if err != nil || !reflect.DeepEqual(built["payload"], payload) {
		return false
	}
	for _, name := range []string{"task_id", "project_key", "session_id", "repo", "branch", "agent_id", "agent_type"} {
		if !reflect.DeepEqual(built[name], event[name]) {
			return false
		}
	}
	return true
}

func asString(value any) string {
	s, _ := value.(string)
	return s
}

// appendObservationOnce repairs a torn final line and de-duplicates by event
// identity before appending. A valid prior record also covers crash recovery
// when its marker was persisted before the spool file was removed.
func appendObservationOnce(path string, line []byte, eventID string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lastLine := strings.LastIndexByte(string(data), '\n') + 1
			tail := data[lastLine:]
			var prior map[string]any
			if json.Unmarshal(tail, &prior) == nil && prior["event_id"] == eventID {
				f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
				if openErr != nil {
					return openErr
				}
				if _, openErr = f.Write([]byte{'\n'}); openErr == nil {
					openErr = f.Sync()
				}
				closeErr := f.Close()
				if openErr != nil {
					return openErr
				}
				if closeErr != nil {
					return closeErr
				}
				return nil
			}
			f, openErr := os.OpenFile(path, os.O_WRONLY, 0600)
			if openErr != nil {
				return openErr
			}
			if openErr = f.Truncate(int64(lastLine)); openErr == nil {
				openErr = f.Sync()
			}
			closeErr := f.Close()
			if openErr != nil {
				return openErr
			}
			if closeErr != nil {
				return closeErr
			}
			data = data[:lastLine]
		}
		for _, record := range strings.Split(string(data), "\n") {
			var prior map[string]any
			if record != "" && json.Unmarshal([]byte(record), &prior) == nil && prior["event_id"] == eventID {
				return nil
			}
		}
	}
	return appendObservationRecord(path, line[:len(line)-1])
}

func appendObservationRecord(path string, encoded []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	line := append(append([]byte(nil), encoded...), '\n')
	_, err = f.Write(line)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func atomicObservationJSON(path string, encoded []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(encoded); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}

func mergeFields(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, set := range sets {
		for name := range set {
			out[name] = true
		}
	}
	return out
}

var observationRules = map[string]string{}

const claudeUsageSnapshotLimit = 16 << 10

var claudeUsagePayloadFields = fields("claude_usage_session_id claude_usage_captured_at claude_5h_start_pct claude_5h_end_pct claude_5h_resets_at claude_7d_start_pct claude_7d_end_pct claude_7d_resets_at claude_context_used_percentage claude_context_window_size claude_context_total_tokens claude_context_input_tokens claude_context_output_tokens claude_cache_creation_input_tokens claude_cache_read_input_tokens claude_session_estimated_cost_usd claude_session_total_duration_ms claude_usage_known claude_rate_limit_delta_scope")

func readClaudeUsagePayload(stateRoot string, now time.Time) map[string]any {
	unknown := map[string]any{"claude_usage_known": false, "claude_rate_limit_delta_scope": "account_window_observation"}
	file, err := os.Open(filepath.Join(stateRoot, "telemetry", "latest", "claude_usage.json"))
	if err != nil {
		return unknown
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, claudeUsageSnapshotLimit+1))
	if err != nil || len(data) > claudeUsageSnapshotLimit {
		return unknown
	}
	var snapshot map[string]any
	if json.Unmarshal(data, &snapshot) != nil || snapshot == nil || snapshot["schema_version"] != float64(1) {
		return unknown
	}
	capturedText, ok := snapshot["captured_at"].(string)
	capturedAt, valid := parseClaudeUsageTimestamp(capturedText)
	if !ok || !valid || capturedAt.After(now) || now.Sub(capturedAt) > 5*time.Minute {
		return unknown
	}
	payload := map[string]any{"claude_usage_captured_at": capturedText, "claude_usage_known": false, "claude_rate_limit_delta_scope": "account_window_observation"}
	if session, ok := snapshot["session_id"].(string); ok && observationID.MatchString(session) {
		payload["claude_usage_session_id"] = session
	}
	for _, field := range []struct{ source, target string }{
		{"context_used_percentage", "claude_context_used_percentage"},
		{"context_window_size", "claude_context_window_size"},
		{"context_total_tokens", "claude_context_total_tokens"},
		{"context_input_tokens", "claude_context_input_tokens"},
		{"context_output_tokens", "claude_context_output_tokens"},
		{"cache_creation_input_tokens", "claude_cache_creation_input_tokens"},
		{"cache_read_input_tokens", "claude_cache_read_input_tokens"},
		{"session_estimated_cost_usd", "claude_session_estimated_cost_usd"},
		{"session_total_duration_ms", "claude_session_total_duration_ms"},
	} {
		if number := validClaudeNumber(snapshot[field.source]); number != nil {
			payload[field.target], payload["claude_usage_known"] = number, true
		}
	}
	for _, window := range []struct{ name, prefix string }{{"five_hour", "claude_5h"}, {"seven_day", "claude_7d"}} {
		values, _ := snapshot[window.name].(map[string]any)
		if values == nil {
			continue
		}
		if number := validClaudeNumber(values["used_percentage"]); number != nil {
			payload[window.prefix+"_start_pct"], payload["claude_usage_known"] = number, true
		}
		if reset, ok := values["resets_at"].(string); ok {
			if _, valid := parseClaudeUsageTimestamp(reset); valid {
				payload[window.prefix+"_resets_at"] = reset
			}
		}
	}
	return payload
}

func parseClaudeUsageTimestamp(value string) (time.Time, bool) {
	if len(value) > 40 {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	_, offset := parsed.Zone()
	return parsed, offset == 0
}

func validClaudeNumber(value any) any {
	number, ok := value.(float64)
	if !ok || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return number
}

func init() {
	setObservationRule("bool", "routing_enforced fresh_thread reply_call claude_usage_known")
	setObservationRule("int", "worker_prompt_chars")
	setObservationRule("number", "queue_wait_ms claude_5h_start_pct claude_5h_end_pct claude_7d_start_pct claude_7d_end_pct claude_context_used_percentage claude_context_window_size claude_context_total_tokens claude_context_input_tokens claude_context_output_tokens claude_cache_creation_input_tokens claude_cache_read_input_tokens claude_session_estimated_cost_usd claude_session_total_duration_ms")
	setObservationRule("enum:implementation operation plan", "phase")
	setObservationRule("enum:mechanical subtle normal consequential escalated operation plan_standard", "route previous_route new_route")
	setObservationRule("enum:override route", "model_source")
	setObservationRule("enum:low medium high xhigh none", "effort")
	setObservationRule("enum:new_task continue fix route_change reanchor repeated_failure user_override", "reason")
	setObservationRule("enum:explicit inherited", "reason_source")
	setObservationRule("enum:implementation operation plan unknown counting_status", "task_type")
	setObservationRule("id", "implementer agent provider claude_usage_session_id job_id")
	setObservationRule("enum:denied queued promoted applied resolved_by_hand discarded expired", "action")
	setObservationRule("time", "claude_usage_captured_at claude_5h_resets_at claude_7d_resets_at")
	setObservationRule("scope", "claude_rate_limit_delta_scope")
}

func setObservationRule(rule, names string) {
	for _, name := range strings.Fields(names) {
		observationRules[name] = rule
	}
}

func normalizeObservationID(value any, fallback string) string {
	original, _ := value.(string)
	clean := strings.Trim(unsafeObservationID.ReplaceAllString(original, "_"), ". ")
	if len(clean) > 96 {
		clean = clean[:96]
	}
	changed := clean != original || clean == ""
	base := strings.ToUpper(strings.SplitN(clean, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		changed, clean = true, "_"+clean
	}
	if clean == "" {
		clean = fallback
	}
	if changed {
		sum := sha256.Sum256([]byte(original))
		if len(clean) > 87 {
			clean = clean[:87]
		}
		clean += "-" + hex.EncodeToString(sum[:4])
	}
	return clean
}

func buildObservation(eventType string, envelope, payload map[string]any, taskID any) (map[string]any, error) {
	return buildObservationAtStateRoot(eventType, envelope, payload, taskID, "")
}

func buildObservationAtStateRoot(eventType string, envelope, payload map[string]any, taskID any, stateRoot string) (map[string]any, error) {
	allowed, ok := observationFields[eventType]
	if !ok {
		return nil, fmt.Errorf("unknown telemetry event type")
	}
	if eventType == "IMPLEMENTER_STARTED" && stateRoot != "" {
		callerPayload := payload
		payload = make(map[string]any, len(callerPayload)+2)
		for name, value := range callerPayload {
			payload[name] = value
		}
		for name := range claudeUsagePayloadFields {
			delete(payload, name)
		}
		for name, value := range readClaudeUsagePayload(stateRoot, time.Now().UTC()) {
			payload[name] = value
		}
	}
	clean := map[string]any{}
	for name, value := range payload {
		if !allowed[name] {
			return nil, fmt.Errorf("telemetry payload contains an unregistered field")
		}
		if value == nil && name != "eta_minutes" {
			continue
		}
		if err := validateObservationValue(name, value); err != nil {
			return nil, err
		}
		clean[name] = value
	}
	project := envelope["project_key"]
	if project == nil && taskID != nil {
		project = "unresolved"
	}
	if project != nil {
		p, ok := project.(string)
		if !ok || !observationProjectKey.MatchString(p) {
			return nil, fmt.Errorf("telemetry project key is invalid")
		}
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return map[string]any{"schema_version": observationSchemaVersion, "event_id": fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), "ts_utc": time.Now().UTC().Format(time.RFC3339Nano), "event_type": eventType,
		"task_id": optionalObservationID(taskID, "unknown"), "project_key": project, "session_id": optionalObservationID(envelope["session_id"], "session"), "repo": optionalObservationID(envelope["repo"], "repo"), "branch": optionalObservationID(envelope["branch"], "branch"), "agent_id": optionalObservationID(envelope["agent_id"], "agent"), "agent_type": optionalObservationID(envelope["agent_type"], "agent"), "payload": clean}, nil
}

func optionalObservationID(value any, fallback string) any {
	if value == nil {
		return nil
	}
	return normalizeObservationID(value, fallback)
}
func validNumber(value any, integer bool) bool {
	var n float64
	switch x := value.(type) {
	case float64:
		n = x
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	default:
		return false
	}
	return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) && (!integer || math.Trunc(n) == n)
}
func oneOf(value any, values string) bool {
	for _, item := range strings.Fields(values) {
		if value == item {
			return true
		}
	}
	return false
}

func validateObservationValue(name string, value any) error {
	rule := observationRules[name]
	valid := false
	switch {
	case rule == "bool":
		_, valid = value.(bool)
	case rule == "int":
		valid = validNumber(value, true)
	case rule == "number":
		valid = validNumber(value, false)
	case rule == "id":
		s, ok := value.(string)
		valid = ok && observationID.MatchString(s)
	case rule == "time":
		s, ok := value.(string)
		parsed, err := time.Parse(time.RFC3339Nano, s)
		_, offset := parsed.Zone()
		valid = ok && len(s) <= 40 && err == nil && offset == 0
	case strings.HasPrefix(rule, "enum:"):
		valid = oneOf(value, strings.TrimPrefix(rule, "enum:"))
	case name == "model":
		s, ok := value.(string)
		valid = ok && modelValue.MatchString(s)
	case name == "violation_code":
		valid = oneOf(value, "MALFORMED_TOOL_INPUT MISSING_ORCH_LINE MALFORMED_ORCH_LINE UNKNOWN_KEY MISSING_REQUIRED_KEY INVALID_ENUM_VALUE INVALID_SCOPE VAGUE_SOL_BASIS UNBOUNDED_SCOPE CODEX_REPLY_INPUT_INVALID THREAD_ID_MISMATCH TASK_ID_MISMATCH ROUTE_MISMATCH MODEL_MISMATCH EFFORT_MISMATCH THREAD_TASK_MISMATCH THREAD_PHASE_MISMATCH THREAD_ROUTE_MISMATCH USE_CODEX_REPLY NO_LIVE_THREAD INVALID_LIVE_THREAD INVALID_REQUEST SCOPE_LEASE_CONFLICT LEASE_ADMISSION_UNCERTAIN LEASE_OWNER_CONFLICT LEASE_INVALID")
	case name == "eta_minutes":
		valid = value == nil || validNumber(value, true) && oneOfNumeric(value, 1, 600)
	case name == "suspected_route_mismatch":
		m, ok := value.(map[string]any)
		valid = ok && len(m) == 2 && oneOf(m["declared_route"], "mechanical subtle normal consequential escalated operation plan_standard") && oneOf(m["derived_type"], "counting_status operation")
	case name == "claude_rate_limit_delta_scope":
		valid = value == "account_window_observation"
	}
	if !valid {
		return fmt.Errorf("invalid telemetry field %q", name)
	}
	return nil
}

func oneOfNumeric(value any, min, max float64) bool {
	switch n := value.(type) {
	case int:
		return float64(n) >= min && float64(n) <= max
	case float64:
		return n >= min && n <= max
	}
	return false
}

var healthErrorCodes = fields("TELEMETRY_ENTRYPOINT_FAILURE TELEMETRY_EMIT_FAILED TELEMETRY_UPDATE_TASK_FAILED TELEMETRY_REBUILD_FAILED TELEMETRY_HEALTH_FAILED TELEMETRY_SPOOL_MERGE_FAILED TELEMETRY_PROJECTION_FAILED MALFORMED_SPOOL_EVENT CLAUDE_TRANSCRIPT_PARSER_UNAVAILABLE CLAUDE_TRANSCRIPT_PARSER_FAILED CLAUDE_TRANSCRIPT_FINALIZATION_FAILED CONTEXT_IO_EMIT_FAILED")

// recordObservationHealth is best effort: health IO cannot affect enforcement.
func recordObservationHealth(root, status, errorCode string) {
	defer func() { _ = recover() }()
	path := filepath.Join(root, "telemetry", "telemetry_health.json")
	data, err := os.ReadFile(path)
	health := map[string]any{"schema_version": 1, "status": "OK", "failure_count": 0, "last_error_code": nil}
	if err == nil {
		var saved map[string]any
		if json.Unmarshal(data, &saved) == nil && saved != nil {
			health = saved
		}
	}
	if status == "DEGRADED" {
		if !healthErrorCodes[errorCode] {
			errorCode = "TELEMETRY_HEALTH_FAILED"
		}
		count, ok := health["failure_count"].(float64)
		if !ok {
			if n, yes := health["failure_count"].(int); yes {
				count, ok = float64(n), true
			} else if value, yes := health["failure_count"].(bool); yes {
				if value {
					count = 1
				}
				ok = true
			}
		}
		if !ok || count < 0 || math.Trunc(count) != count {
			count = 0
		}
		health["schema_version"], health["status"], health["failure_count"], health["last_error_code"] = 1, status, count+1, errorCode
	} else if status == "OK" {
		health["schema_version"], health["status"], health["last_error_code"] = 1, status, nil
	} else {
		return
	}
	health["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err = json.Marshal(health)
	if err != nil {
		return
	}
	_ = atomicWrite(path, data)
}
