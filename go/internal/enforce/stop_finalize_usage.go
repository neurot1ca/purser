package enforce

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxTaskUsageToken = 100_000_000

func finalizeStopTaskUsage(decision, active map[string]any, session, transcriptPath, projectsRoot string) map[string]any {
	if decision["status"] != "READY" {
		return decision
	}
	if transcriptPath == "" {
		transcriptPath, _ = active["canonical_transcript_path"].(string)
	}
	resolved, reason := validateTranscriptPath(transcriptPath, session, projectsRoot)
	if reason != "" {
		return stopFinalizeFailed(reason)
	}
	identity, reason := snapshotTranscript(resolved)
	if reason != "" {
		return stopFinalizeFailed(reason)
	}
	if reason = checkStopFinalizeRecordIdentity(identity, active); reason != "" {
		return stopFinalizeFailed(reason)
	}

	offset := int64(0)
	if active["anchor_deferred"] != true {
		if n, ok := taskUsageOffset(active["anchor_byte_offset"]); ok {
			offset = n
		} else if n, ok := taskUsageOffset(active["anchor_search_start_offset"]); ok {
			offset = n
		}
	} else if n, ok := taskUsageOffset(active["anchor_search_start_offset"]); ok {
		offset = n
	}
	file, err := os.Open(resolved)
	if err != nil {
		return stopFinalizeFailed("TRANSCRIPT_UNAVAILABLE")
	}
	records, reason := readTaskUsageRecords(file, offset, false)
	_ = file.Close()
	if reason != "" {
		return stopFinalizeFailed(reason)
	}
	toolUseID, _ := active["tool_use_id"].(string)
	usage, reason := aggregateStopTaskUsage(records, toolUseID)
	if reason != "" {
		return stopFinalizeFailed(reason)
	}
	return map[string]any{"status": "COMPLETED", "usage": usage}
}

func stopFinalizeFailed(reason string) map[string]any {
	return map[string]any{"status": "FAILED", "reason": reason}
}

func checkStopFinalizeRecordIdentity(current *transcriptIdentity, active map[string]any) string {
	if expected, ok := active["canonical_transcript_path"].(string); ok && expected != "" {
		expected = filepath.Clean(expected)
		currentPath := filepath.Clean(current.path)
		if runtime.GOOS == "windows" {
			expected, currentPath = strings.ToLower(expected), strings.ToLower(currentPath)
		}
		if currentPath != expected {
			return "TRANSCRIPT_IDENTITY_MISMATCH"
		}
	}
	dev, ino, ctime := taskUsageFileIdentity(current.info)
	if expected := active["transcript_dev"]; expected != nil && dev != nil && !sameTaskUsageInt(expected, dev) {
		return "TRANSCRIPT_IDENTITY_MISMATCH"
	}
	if expected := active["transcript_ino"]; expected != nil && !sameTaskUsageInt(expected, int64(0)) && ino != nil && !sameTaskUsageInt(expected, ino) {
		return "TRANSCRIPT_IDENTITY_MISMATCH"
	}
	if runtime.GOOS == "windows" {
		if expected := active["transcript_ctime_ns"]; expected != nil && ctime != nil && !sameTaskUsageInt(expected, ctime) {
			return "TRANSCRIPT_IDENTITY_MISMATCH"
		}
	}
	initial, _ := taskUsageOffset(active["initial_size"])
	if current.size < initial {
		return "TRANSCRIPT_ROTATED_OR_TRUNCATED"
	}
	if anchor, ok := taskUsageOffset(active["anchor_byte_offset"]); ok && anchor > current.size {
		return "TRANSCRIPT_ROTATED_OR_TRUNCATED"
	}
	if cursor, ok := taskUsageOffset(active["anchor_search_start_offset"]); ok && cursor > current.size {
		return "TRANSCRIPT_ROTATED_OR_TRUNCATED"
	}
	return ""
}

func sameTaskUsageInt(value, expected any) bool {
	if n, ok := value.(float64); ok {
		if want, ok := expected.(int64); ok {
			return n == float64(want)
		}
	}
	if want, ok := expected.(float64); ok {
		if n, ok := value.(int64); ok {
			return want == float64(n)
		}
	}
	n, ok := taskUsageOffset(value)
	want, wantOK := taskUsageOffset(expected)
	return ok && wantOK && n == want
}

func taskUsageOffset(value any) (int64, bool) {
	switch n := value.(type) {
	case int64:
		return n, n >= 0
	case float64:
		return int64(n), n >= 0 && n == math.Trunc(n)
	default:
		return 0, false
	}
}

func aggregateStopTaskUsage(records []map[string]any, toolUseID string) (map[string]any, string) {
	anchor := -1
	for i, record := range records {
		message, _ := record["message"].(map[string]any)
		content, _ := message["content"].([]any)
		for _, value := range content {
			block, _ := value.(map[string]any)
			if block["type"] == "tool_use" && block["id"] == toolUseID {
				anchor = i
				break
			}
		}
		if anchor >= 0 {
			break
		}
	}
	if anchor < 0 {
		return nil, "ANCHOR_NOT_FOUND"
	}
	type dedupKey struct{ message, request string }
	seen := make(map[dedupKey][4]int64)
	var in, out, create, read int64
	for _, record := range records[anchor:] {
		message, ok := record["message"].(map[string]any)
		if !ok {
			return nil, "INVALID_USAGE_RECORD"
		}
		id, ok := message["id"].(string)
		if !ok || id == "" {
			return nil, "INVALID_USAGE_RECORD"
		}
		request := "_msg_fallback_" + id
		if raw, exists := record["requestId"]; exists && raw != nil {
			request, ok = raw.(string)
			if !ok || request == "" {
				return nil, "INVALID_USAGE_RECORD"
			}
		}
		rawUsage, ok := message["usage"].(map[string]any)
		if !ok {
			return nil, "INVALID_USAGE_RECORD"
		}
		values := [4]int64{}
		for i, key := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			value, exists := rawUsage[key]
			if !exists && i >= 2 {
				continue
			}
			n, valid := stopUsageToken(value)
			if !valid {
				return nil, "INVALID_USAGE_RECORD"
			}
			values[i] = n
		}
		key := dedupKey{id, request}
		if previous, exists := seen[key]; exists {
			if previous != values {
				return nil, "CONTRADICTORY_USAGE_RECORD"
			}
			continue
		}
		seen[key] = values
		in += values[0]
		out += values[1]
		create += values[2]
		read += values[3]
	}
	if len(seen) == 0 {
		return nil, "NO_USAGE_RECORDS"
	}
	return map[string]any{
		"claude_task_usage_source": "desktop_jsonl_observed", "claude_task_usage_known": true,
		"claude_task_usage_complete": true, "claude_task_input_tokens": in,
		"claude_task_output_tokens": out, "claude_task_cache_creation_input_tokens": create,
		"claude_task_cache_read_input_tokens": read, "claude_task_total_observed_tokens": in + out + create + read,
		"claude_task_request_count": len(seen), "claude_task_usage_boundary": "anchored_pre_tool_to_stop",
		"claude_task_usage_dedup": "message_id_request_id", "claude_task_usage_unknown_reason": nil,
		"claude_task_subagent_usage_included": false,
	}, ""
}

func stopUsageToken(value any) (int64, bool) {
	n, ok := taskUsageOffset(value)
	return n, ok && n <= maxTaskUsageToken
}
