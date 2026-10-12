package enforce

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"purser-hook/internal/contextio"
)

// Review interval records are local pilot data. They are deliberately kept
// outside bundles, prompts, and task observations so the measurement cannot
// affect enforcement inputs.
func recordReviewIntervalDelivery(root string, event map[string]any, jobID, taskID string, bundle map[string]any) {
	metadata := clonePostMap(event)
	metadata["review_presentation"] = mapValue(bundle["review_navigation"])["presentation"]
	recordReviewIntervalDeliveryAt(root, metadata, jobID, taskID, time.Now().UTC())
}

func recordReviewIntervalDeliveryAt(root string, event map[string]any, jobID, taskID string, delivered time.Time) {
	path, ok := reviewIntervalPath(root, jobID)
	if !ok {
		return
	}
	unlock, err := contextio.LockFile(path + ".lock")
	if err != nil {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
		return
	}
	defer func() { _ = unlock() }()
	interval := map[string]any{}
	if data, readErr := os.ReadFile(path); readErr == nil {
		if json.Unmarshal(data, &interval) != nil {
			recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
			return
		}
	} else if !os.IsNotExist(readErr) {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
		return
	}
	if stringValue(interval["terminal_utc"]) != "" {
		return
	}
	if stringValue(interval["result_delivered_utc"]) == "" {
		interval = map[string]any{
			"schema_version":              1,
			"job_id":                      jobID,
			"task_id":                     taskID,
			"result_delivered_utc":        delivered.UTC().Format(time.RFC3339Nano),
			"decision":                    nil,
			"terminal_utc":                nil,
			"review_seconds":              nil,
			"request_ids":                 []any{},
			"usage_status":                "UNKNOWN",
			"usage_unknown_reason":        "OPEN",
			"input_tokens":                nil,
			"cached_input_tokens":         nil,
			"cache_creation_input_tokens": nil,
			"output_tokens":               nil,
			"transcript_path":             stringValue(event["transcript_path"]),
			"presentation":                event["review_presentation"],
		}
	}
	encoded, err := json.Marshal(interval)
	if err == nil {
		err = atomicObservationJSON(path, encoded)
	}
	if err != nil {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
	}
}

func recordReviewIntervalDecision(root, jobID, decision string) {
	recordReviewIntervalDecisionAt(root, jobID, decision, time.Now().UTC())
}

func recordReviewIntervalDecisionAt(root, jobID, decision string, terminal time.Time) {
	if decision != "accepted" && decision != "send_back" {
		return
	}
	path, ok := reviewIntervalPath(root, jobID)
	if !ok {
		return
	}
	unlock, err := contextio.LockFile(path + ".lock")
	if err != nil {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
		return
	}
	defer func() { _ = unlock() }()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	interval := map[string]any{}
	if json.Unmarshal(data, &interval) != nil {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
		return
	}
	if stringValue(interval["terminal_utc"]) != "" {
		return
	}
	delivered, deliveredErr := time.Parse(time.RFC3339Nano, stringValue(interval["result_delivered_utc"]))
	if deliveredErr != nil || terminal.Before(delivered) {
		interval["usage_status"] = "UNKNOWN"
		interval["usage_unknown_reason"] = "INVALID_INTERVAL_BOUNDARY"
	} else {
		interval["review_seconds"] = terminal.Sub(delivered).Seconds()
		usage := reviewIntervalUsage(stringValue(interval["transcript_path"]), delivered, terminal)
		interval["request_ids"] = usage.requestIDs
		interval["usage_status"] = usage.status
		interval["usage_unknown_reason"] = usage.reason
		interval["input_tokens"] = usage.input
		interval["cached_input_tokens"] = usage.cached
		interval["cache_creation_input_tokens"] = usage.cacheCreation
		interval["output_tokens"] = usage.output
	}
	interval["decision"] = decision
	interval["terminal_utc"] = terminal.UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(interval)
	if err == nil {
		err = atomicObservationJSON(path, encoded)
	}
	if err != nil {
		recordObservationHealth(root, "DEGRADED", "TELEMETRY_EMIT_FAILED")
	}
}

type reviewIntervalUsageResult struct {
	requestIDs, input, cached, cacheCreation, output any
	status                                           string
	reason                                           any
}

func reviewIntervalUsage(path string, from, to time.Time) reviewIntervalUsageResult {
	unknown := func(reason string, ids []any) reviewIntervalUsageResult {
		return reviewIntervalUsageResult{requestIDs: ids, status: "UNKNOWN", reason: reason}
	}
	if path == "" {
		return unknown("TRANSCRIPT_UNAVAILABLE", []any{})
	}
	file, err := os.Open(path)
	if err != nil {
		return unknown("TRANSCRIPT_UNAVAILABLE", []any{})
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	seen := map[string][4]int64{}
	ids := []string{}
	var in, cached, creation, out int64
	unknownReason := ""
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			unknownReason = "INVALID_TRANSCRIPT_RECORD"
			continue
		}
		if record["type"] != "assistant" {
			continue
		}
		stamp, stampErr := time.Parse(time.RFC3339Nano, stringValue(record["timestamp"]))
		if stampErr != nil || stamp.Before(from) || stamp.After(to) {
			if stampErr != nil {
				unknownReason = "AMBIGUOUS_TRANSCRIPT_TIMESTAMP"
			}
			continue
		}
		message := mapValue(record["message"])
		rawUsage, usageOK := message["usage"].(map[string]any)
		requestID := stringValue(record["requestId"])
		if !usageOK || requestID == "" {
			unknownReason = "MISSING_REVIEW_USAGE"
			continue
		}
		values := [4]int64{}
		valid := true
		for i, key := range []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
			value, exists := rawUsage[key]
			if !exists && i >= 2 {
				continue
			}
			n, ok := stopUsageToken(value)
			if !ok {
				valid = false
				break
			}
			values[i] = n
		}
		if !valid {
			unknownReason = "AMBIGUOUS_REVIEW_USAGE"
			continue
		}
		if prior, exists := seen[requestID]; exists {
			if prior != values {
				unknownReason = "CONTRADICTORY_REVIEW_USAGE"
			}
			continue
		}
		seen[requestID] = values
		ids = append(ids, requestID)
		in, out, creation, cached = in+values[0], out+values[1], creation+values[2], cached+values[3]
	}
	if scanner.Err() != nil {
		unknownReason = "TRANSCRIPT_READ_FAILED"
	}
	sort.Strings(ids)
	idValues := make([]any, len(ids))
	for i, id := range ids {
		idValues[i] = id
	}
	if unknownReason != "" {
		return unknown(unknownReason, idValues)
	}
	if len(ids) == 0 {
		return unknown("NO_REVIEW_USAGE", idValues)
	}
	return reviewIntervalUsageResult{requestIDs: idValues, status: "KNOWN", reason: nil, input: in,
		cached: cached, cacheCreation: creation, output: out}
}

func reviewIntervalPath(root, jobID string) (string, bool) {
	if root == "" || jobID == "" || !observationSafeText.MatchString(jobID) {
		return "", false
	}
	return filepath.Join(root, "telemetry", "review_intervals", jobID+".json"), true
}
