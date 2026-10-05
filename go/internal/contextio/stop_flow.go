package contextio

import "strings"

const stopFlowReminder = "Purser: if this is not a real decision or an approval-gated action, start the next part now instead of asking; otherwise keep the question."

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
