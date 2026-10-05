package contextio

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

var stopRetryJobID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var stopRetryDenial = regexp.MustCompile(`(?is)(?:classifier.{0,60}(?:no verdict|den(?:y|ied))|(?:permission|policy).{0,60}den(?:y|ied)|den(?:y|ied).{0,60}(?:permission|classifier|policy))`)
var stopRetryJobSanitize = regexp.MustCompile(`(?i)[0-9a-f]{64}`)

type stopRetryCall struct {
	id, name string
	result   map[string]any
}

// codexRetryReason finds a retryable async result or classifier denial in the
// transcript segment beginning after latestUser. Retry state remains in Python.
func codexRetryReason(records []map[string]any, latestUser int, repository string) string {
	if latestUser < -1 || latestUser >= len(records) || repository == "" {
		return ""
	}
	uses := make([]stopRetryCall, 0)
	usePositions := make(map[string]int)
	results := make(map[string]map[string]any)
	for _, record := range records[latestUser+1:] {
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_use":
				id := str(block["id"])
				if id != "" {
					call := stopRetryCall{id: id, name: str(block["name"])}
					if position, exists := usePositions[id]; exists {
						uses[position] = call
					} else {
						usePositions[id] = len(uses)
						uses = append(uses, call)
					}
				}
			case "tool_result":
				if id := str(block["tool_use_id"]); id != "" {
					results[id] = block
				}
			}
		}
	}

	root := filepath.Join(stateRoot(), "async_jobs")
	projectKey := repositoryProjectKey(resolve(repository))
	for _, use := range uses {
		if shortStopToolName(use.name) != "codex_start" {
			continue
		}
		result := results[use.id]
		if result == nil {
			continue
		}
		data := stopRetryExtractJSON(stopRetryResultText(result))
		id := str(data["jobId"])
		if !stopRetryJobID.MatchString(id) {
			continue
		}
		index, ok := readSmallObject(filepath.Join(root, id+".json"))
		if !ok || index["jobId"] != id || index["project_key"] != projectKey || index["stage"] == "CONSUMED" {
			continue
		}
		job, ok := readSmallObject(filepath.Join(stateRoot(), "codex_exec_mcp", "jobs", id+".json"))
		if !ok || job["jobId"] != id || job["call"] != index["call"] {
			continue
		}
		state := str(job["state"])
		if state == "SUCCEEDED" || state == "FAILED" || state == "CANCELLED" || state == "TIMED_OUT" || state == "EXPIRED" {
			if label := strings.TrimSpace(str(job["label"])); label != "" {
				label = strings.ReplaceAll(strings.ReplaceAll(label, "\r", " "), "\n", " ")
				label = stopRetryJobSanitize.ReplaceAllString(label, "job")
				return "Retry: call codex_result for the finished job " + label + "."
			}
		}
	}

	// Python keeps the latest result per tool name, in order of first use.
	latest := make([]stopRetryCall, 0, 3)
	positions := make(map[string]int)
	for _, use := range uses {
		name := shortStopToolName(use.name)
		if name != "codex_start" && name != "codex_result" && name != "codex_apply" {
			continue
		}
		use.result = results[use.id]
		if i, ok := positions[name]; ok {
			latest[i] = use
		} else {
			positions[name] = len(latest)
			latest = append(latest, use)
		}
	}
	for _, call := range latest {
		if call.result != nil && call.result["is_error"] == true && stopRetryDenial.MatchString(stopRetryResultText(call.result)) {
			return "Retry the denied " + shortStopToolName(call.name) + " call."
		}
	}
	return ""
}

func shortStopToolName(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func stopRetryResultText(result map[string]any) string {
	switch content := result["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			if item, ok := raw.(map[string]any); ok {
				parts = append(parts, str(item["text"]))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func stopRetryExtractJSON(text string) map[string]any {
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) == nil && object != nil {
		return object
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end >= start && json.Unmarshal([]byte(text[start:end+1]), &object) == nil && object != nil {
		return object
	}
	return nil
}
