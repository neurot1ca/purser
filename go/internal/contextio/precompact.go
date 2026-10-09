package contextio

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

const stateStart = "<!-- PURSER:STATE:START -->"
const stateEnd = "<!-- PURSER:STATE:END -->"

var orchestrationTask = regexp.MustCompile(`^ORCH\|task=([^|\s]+)\|`)

func PreCompact(event map[string]any) {
	repository := projectRoot(event)
	if repository == "" {
		return
	}
	path := filepath.Join(stateRoot(), "projects", repositoryProjectKey(repository), "telemetry", "purser-handoff.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	content := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	start, end := strings.Index(content, stateStart), strings.Index(content, stateEnd)
	if start < 0 || end < 0 || start >= end {
		return
	}
	records := readTranscriptRecords(stringValue(event["transcript_path"]))
	body := compactionMechanicalState(repository, records)
	start += len(stateStart)
	content = content[:start] + "\n" + body + "\n" + content[end:]
	content = replaceRecentTurns(content, formatRecentTurns(records))
	_ = writeHandoffAtomically(path, []byte(content))
}

func compactionMechanicalState(repository string, records []map[string]any) string {
	context := 0
	for i := len(records) - 1; i >= 0; i-- {
		if transcriptRecordType(records[i]) != "assistant" {
			continue
		}
		message, _ := records[i]["message"].(map[string]any)
		if message == nil {
			message = records[i]
		}
		usage, _ := message["usage"].(map[string]any)
		for _, key := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
			if value, ok := usage[key].(float64); ok && value >= 0 && value == math.Trunc(value) {
				context += int(value)
			}
		}
		break
	}
	triggers, calls := compactionTranscriptFacts(records)
	lines := []string{
		"as of " + time.Now().UTC().Format("2006-01-02T15:04Z"),
		fmt.Sprintf("Context tokens: %d [transcript]", context),
		fmt.Sprintf("Triggers: %s [transcript]", strings.Join(triggers, ", ")),
		"Last Codex calls:",
	}
	for _, call := range calls[max(0, len(calls)-5):] {
		thread, _ := call["threadId"].(string)
		if thread == "" {
			thread = "unknown"
		}
		review, _ := call["claude_review_mode"].(string)
		if review == "" {
			review = "unknown"
		}
		lines = append(lines, fmt.Sprintf("- task=%s tool=%s threadId=%s claude_review_mode=%s", call["task_id"], call["tool"], thread, review))
	}
	if plan := renderActivePlanPointer(repository); plan != "" {
		lines = append(lines, plan)
	}
	if output, err := runGit(repository, "log", "--oneline", "-5"); err == nil {
		lines = append(lines, "Last commits:")
		for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			if line != "" && len(lines) < 12 {
				lines = append(lines, "- "+line)
			}
		}
	}
	if output, err := runGit(repository, "status", "--porcelain"); err == nil {
		changed := 0
		for _, line := range strings.Split(output, "\n") {
			if strings.TrimSpace(line) != "" {
				changed++
			}
		}
		lines = append(lines, fmt.Sprintf("Uncommitted changed paths: %d [git status]", changed))
	}
	body := strings.Join(lines, "\n")
	runes := []rune(body)
	if len(runes) > 1500 {
		body = trimPythonWhitespace(string(runes[:1486])) + "\n... truncated"
	}
	return body
}

func compactionTranscriptFacts(records []map[string]any) ([]string, []map[string]any) {
	uses := map[string]map[string]any{}
	useIndex := map[string]int{}
	results := map[string]map[string]any{}
	resultIndex := map[string]int{}
	latestUser := -1
	for i, record := range records {
		if genuineTranscriptUser(record) {
			latestUser = i
		}
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := block["id"].(string)
			if block["type"] == "tool_use" && id != "" {
				uses[id] = block
				useIndex[id] = i
			}
			toolID, _ := block["tool_use_id"].(string)
			if block["type"] == "tool_result" && toolID != "" {
				results[toolID], resultIndex[toolID] = block, i
			}
		}
	}
	segmentStart := latestUser + 1
	var triggers []string
	for i := len(records) - 1; i >= segmentStart; i-- {
		if transcriptRecordType(records[i]) != "assistant" {
			continue
		}
		if stamp, ok := transcriptTimestamp(records[i]); ok && time.Since(stamp) >= time.Duration(compactionIdleMinutes())*time.Minute {
			triggers = append(triggers, "idle")
		}
		break
	}
	commit := false
	for id, use := range uses {
		if _, exists := results[id]; !exists || results[id]["is_error"] == true || useIndex[id] < segmentStart {
			continue
		}
		name, _ := use["name"].(string)
		input, _ := use["input"].(map[string]any)
		command, _ := input["command"].(string)
		if (name == "Bash" || name == "PowerShell") && strings.Contains(strings.ToLower(command), "git commit") && resultIndex[id] >= segmentStart {
			commit = true
		}
	}
	if commit {
		triggers = append(triggers, "commit")
	}
	calls := []map[string]any{}
	for _, record := range records {
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			use := uses[id]
			name, _ := use["name"].(string)
			tool := map[string]string{"mcp__plugin_purser_codex-main__codex": "codex", "mcp__plugin_purser_codex-main__codex-reply": "codex-reply"}[name]
			if tool == "" {
				continue
			}
			input, _ := use["input"].(map[string]any)
			prompt, _ := input["prompt"].(string)
			task := ""
			if match := orchestrationTask.FindStringSubmatch(prompt); len(match) == 2 {
				task = match[1]
			}
			result, exists := results[id]
			if !exists {
				continue
			}
			data := extractResultJSON(result["content"])
			if task == "" {
				task, _ = data["task_id"].(string)
			}
			if task == "" {
				task = "unknown"
			}
			thread := data["threadId"]
			if thread == nil || thread == "" {
				thread = data["thread_id"]
			}
			calls = append(calls, map[string]any{"task_id": task, "tool": tool, "threadId": thread, "claude_review_mode": data["claude_review_mode"]})
		}
	}
	return triggers, calls
}

func transcriptBlocks(record map[string]any) []any {
	message, _ := record["message"].(map[string]any)
	if message == nil {
		message = record
	}
	blocks, _ := message["content"].([]any)
	return blocks
}

func genuineTranscriptUser(record map[string]any) bool {
	if transcriptRecordType(record) != "user" || record["isMeta"] == true || record["isCompactSummary"] == true {
		return false
	}
	for _, raw := range transcriptBlocks(record) {
		if block, ok := raw.(map[string]any); ok && block["type"] == "tool_result" {
			return false
		}
	}
	message, _ := record["message"].(map[string]any)
	if message == nil {
		message = record
	}
	text := strings.TrimLeftFunc(transcriptText(message["content"]), isPythonWhitespace)
	return text != "" && !strings.HasPrefix(text, "<task-notification>") && !strings.HasPrefix(text, "[SYSTEM NOTIFICATION - NOT USER INPUT]")
}

func transcriptTimestamp(record map[string]any) (time.Time, bool) {
	stamp, _ := record["timestamp"].(string)
	message, _ := record["message"].(map[string]any)
	if stamp == "" {
		stamp, _ = message["timestamp"].(string)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, stamp); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func compactionIdleMinutes() int {
	minutes := 20
	home, err := os.UserHomeDir()
	if err != nil {
		return minutes
	}
	data, err := os.ReadFile(filepath.Join(home, ".orchestrator", "config.toml"))
	if err != nil {
		return minutes
	}
	section := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line == "[compaction]"
			continue
		}
		if section && strings.HasPrefix(line, "idle_minutes") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				if value, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil && value > 0 {
					return value
				}
			}
		}
	}
	return minutes
}

func extractResultJSON(value any) map[string]any {
	text := ""
	switch content := value.(type) {
	case string:
		text = content
	case []any:
		for _, raw := range content {
			if block, ok := raw.(map[string]any); ok {
				if part, ok := block["text"].(string); ok {
					text += part + "\n"
				}
			}
		}
	}
	var data map[string]any
	if json.Unmarshal([]byte(text), &data) == nil && data != nil {
		return data
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end > start && json.Unmarshal([]byte(text[start:end+1]), &data) == nil && data != nil {
		return data
	}
	return map[string]any{}
}

func renderActivePlanPointer(repository string) string {
	plans, err := ReconcilePlans(stateRoot(), repository)
	if err != nil || len(plans) == 0 {
		return ""
	}
	if len(plans) == 1 {
		state := plans[0]
		if state.UserStopped || !containsMilestone(state.AcceptedMilestones, state.CurrentMilestone) {
			return ""
		}
		action := state.PendingNextAction
		if state.Gate != "" {
			action = "gate: " + state.Gate
		}
		if action == "" {
			return ""
		}
		return fmt.Sprintf("Plan %s: milestone %d of %d; %d applied passes; next action: %s; plan: %s.", state.TaskID, state.CurrentMilestone, len(state.AcceptedMilestones), len(state.AppliedPasses), action, state.ArtifactPath)
	}
	path, _, err := planPath(stateRoot(), repository)
	if err != nil {
		return ""
	}
	lines := make([]string, 0, len(plans)+2)
	lines = append(lines, "Unfinished plans:")
	for _, plan := range plans {
		action := plan.PendingNextAction
		if plan.UserStopped {
			action = "stopped"
		} else if plan.Gate != "" {
			action = "gate: " + plan.Gate
		}
		if !containsMilestone(plan.AcceptedMilestones, plan.CurrentMilestone) {
			action = "invalid milestone"
		}
		lines = append(lines, fmt.Sprintf("%s M%d/%d %s", plan.TaskID, plan.CurrentMilestone, len(plan.AcceptedMilestones), action))
	}
	lines = append(lines, "Details: purser-hook plan status --task-id <id>.")
	stateLine := "State: " + path
	const limit = 900
	text := strings.Join(lines, "\n")
	if len([]rune(text+"\n"+stateLine)) > limit {
		kept := make([]string, 0, len(plans))
		for i, line := range lines[1 : len(lines)-1] {
			candidate := append(append([]string(nil), kept...), line)
			omitted := len(plans) - i - 1
			candidate = append(candidate, fmt.Sprintf("... %d plans omitted", omitted), lines[len(lines)-1], stateLine)
			if len([]rune(strings.Join(candidate, "\n"))) > limit {
				break
			}
			kept = append(kept, line)
		}
		omitted := len(plans) - len(kept)
		lines = append([]string{"Unfinished plans:"}, kept...)
		lines = append(lines, fmt.Sprintf("... %d plans omitted", omitted), "Details: purser-hook plan status --task-id <id>.", stateLine)
	}
	return strings.Join(lines, "\n")
}

func deliverPlanPointer(repository, statePath, session string, sessionStart bool) string {
	if repository == "" || statePath == "" || session == "" {
		return ""
	}
	pointer := renderActivePlanPointer(repository)
	if os.MkdirAll(filepath.Dir(statePath), 0700) != nil {
		return ""
	}
	lock, err := os.OpenFile(statePath+".injections.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return ""
	}
	defer lock.Close()
	if tryLock(lock) != nil {
		return ""
	}
	defer unlock(lock)
	state := readAdvisorState(statePath, session)
	previous, _ := state["last_plan_pointer"].(string)
	if pointer == "" {
		if previous != "" {
			delete(state, "last_plan_pointer")
			_ = writeAdvisorState(statePath, state)
		}
		return ""
	}
	if !sessionStart && pointer == previous {
		return ""
	}
	state["last_plan_pointer"] = pointer
	if writeAdvisorState(statePath, state) != nil {
		return ""
	}
	return pointer
}

func transcriptRecordType(record map[string]any) string {
	if raw, exists := record["type"]; exists {
		kind, _ := raw.(string)
		return strings.ToLower(kind)
	}
	message, _ := record["message"].(map[string]any)
	role, _ := message["role"].(string)
	return strings.ToLower(role)
}

func replaceRecentTurns(content, recent string) string {
	if start := strings.Index(content, recentStart); start >= 0 {
		if end := strings.Index(content[start:], recentEnd); end >= 0 {
			end += start + len(recentEnd)
			content = content[:start] + content[end:]
		}
	}
	if recent != "" {
		content = strings.TrimRightFunc(content, isPythonWhitespace) + "\n" + recentStart + "\n" + recent + "\n" + recentEnd + "\n"
	}
	return content
}

func runGit(repository string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repository
	proc.ConfigureNoWindow(command)
	output, err := command.Output()
	return string(output), err
}

func writeHandoffAtomically(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err = temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return replaceWithRetry(name, path)
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
