package contextio

import "strings"

const stopFlowReminder = "Purser: if this is not a real decision or an approval-gated action, start the next part now instead of asking; otherwise keep the question."

func planCodexDispatched(records []map[string]any, latestUser int, planID string) bool {
	if latestUser < 0 || latestUser >= len(records) || planID == "" {
		return false
	}
	for _, record := range records[latestUser+1:] {
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			switch shortStopToolName(str(block["name"])) {
			case "codex_start":
				if planTaskInDispatchInput(block["input"], planID) {
					return true
				}
			case "Agent":
				input, ok := block["input"].(map[string]any)
				if ok && input["subagent_type"] == "purser:dispatcher" && planTaskInDispatchInput(input, planID) {
					return true
				}
			}
		}
	}
	return false
}

func planMilestoneDispatched(records []map[string]any, latestUser int, planID string, milestone int) bool {
	if latestUser < 0 || latestUser >= len(records) || planID == "" || milestone <= 0 {
		return false
	}
	for _, record := range records[latestUser+1:] {
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			dispatcher := false
			switch shortStopToolName(str(block["name"])) {
			case "codex_start":
				dispatcher = true
			case "Agent":
				input, ok := block["input"].(map[string]any)
				dispatcher = ok && input["subagent_type"] == "purser:dispatcher"
			}
			if dispatcher && planMilestoneInDispatchInput(block["input"], planID, milestone) {
				return true
			}
		}
	}
	return false
}

func planMilestoneInDispatchInput(value any, planID string, milestone int) bool {
	switch input := value.(type) {
	case map[string]any:
		for _, child := range input {
			if planMilestoneInDispatchInput(child, planID, milestone) {
				return true
			}
		}
	case []any:
		for _, child := range input {
			if planMilestoneInDispatchInput(child, planID, milestone) {
				return true
			}
		}
	case string:
		if n, ok := planTaskMilestone(planID, input); ok && n == milestone {
			return true
		}
		for _, marker := range strings.Split(input, "ORCH|task=")[1:] {
			fields := strings.FieldsFunc(marker, func(r rune) bool { return r == '|' || r == ' ' || r == '\t' || r == '\r' || r == '\n' })
			if len(fields) > 0 {
				if n, ok := planTaskMilestone(planID, fields[0]); ok && n == milestone {
					return true
				}
			}
		}
	}
	return false
}

func planTaskInDispatchInput(value any, planID string) bool {
	switch input := value.(type) {
	case map[string]any:
		for key, child := range input {
			if key == "task_id" || key == "taskId" {
				if taskID, ok := child.(string); ok && PlanTaskMatches(planID, taskID) {
					return true
				}
			}
			if planTaskInDispatchInput(child, planID) {
				return true
			}
		}
	case []any:
		for _, child := range input {
			if planTaskInDispatchInput(child, planID) {
				return true
			}
		}
	case string:
		if PlanTaskMatches(planID, input) {
			return true
		}
		for _, marker := range strings.Split(input, "ORCH|task=")[1:] {
			fields := strings.FieldsFunc(marker, func(r rune) bool { return r == '|' || r == ' ' || r == '\t' || r == '\r' || r == '\n' })
			if len(fields) == 0 {
				continue
			}
			taskID := fields[0]
			if PlanTaskMatches(planID, taskID) {
				return true
			}
		}
	}
	return false
}

// stopFlowReason mirrors the question guard in compaction_advisor.stop. It is
// considered only when no other Stop reason was selected.
func stopFlowReason(records []map[string]any, latestUser, blockedUser int, hasReasons bool) (string, int) {
	if hasReasons || latestUser < 0 || latestUser >= len(records) || blockedUser == latestUser {
		return "", blockedUser
	}
	flowMessage := ""
	for i := len(records) - 1; i >= 0; i-- {
		if transcriptRecordType(records[i]) == "assistant" {
			message, _ := records[i]["message"].(map[string]any)
			if message == nil {
				message = records[i]
			}
			flowMessage = transcriptText(message["content"])
			break
		}
	}
	trimmed := strings.TrimRightFunc(flowMessage, isPythonWhitespace)
	if !strings.HasSuffix(trimmed, "?") && !strings.HasSuffix(trimmed, "؟") && !strings.HasSuffix(trimmed, "？") {
		return "", blockedUser
	}
	uses := make(map[string]string)
	for _, record := range records[latestUser+1:] {
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if id, ok := block["id"].(string); ok {
					uses[id] = str(block["name"])
				}
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				name := uses[id]
				delete(uses, id)
				if strings.HasSuffix(name, "__codex_result") && block["is_error"] != true {
					return stopFlowReminder, latestUser
				}
			}
		}
	}
	return "", blockedUser
}

// stopReasons preserves Python's ordering: retry first, then advisor reasons;
// the flow reminder is eligible only when those sources supplied no reason.
func stopReasons(retry string, advisor []string, flow string) []string {
	reasons := make([]string, 0, len(advisor)+2)
	if retry != "" {
		reasons = append(reasons, retry)
	}
	for _, reason := range advisor {
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) == 0 && flow != "" {
		reasons = append(reasons, flow)
	}
	return reasons
}
