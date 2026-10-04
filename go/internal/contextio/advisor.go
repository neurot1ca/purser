package contextio

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const postCompactContextTokens = 75000

var advisorTask = regexp.MustCompile(`^ORCH\|task=([^|\s]+)\|`)

func queueFallbackQuestion(event map[string]any, repository, line, model string, notice bool) bool {
	return queueFallbackQuestionAtRoot(event, repository, line, model, notice, stateRoot())
}

func queueFallbackQuestionAtRoot(event map[string]any, repository, line, model string, notice bool, root string) bool {
	session := str(event["session_id"])
	if repository == "" || session == "" {
		return false
	}
	path := filepath.Join(root, "projects", repositoryProjectKey(repository), "telemetry", "compaction_advisor.json")
	state := readAdvisorState(path, session)
	old, _ := state["fallback_pending"].(map[string]any)
	delivered := old != nil && old["line"] == line && old["model"] == model && old["delivered"] == true
	state["fallback_pending"] = map[string]any{
		"line": line, "project": repository, "model": model,
		"blocked": false, "notice": notice, "delivered": delivered,
	}
	return writeAdvisorState(path, state) == nil
}

// QueueFallbackQuestion persists the question for SessionStart delivery.
func QueueFallbackQuestion(event map[string]any, repository, line, model string, notice bool, root string) bool {
	return queueFallbackQuestionAtRoot(event, repository, line, model, notice, root)
}

func updateFallbackQuestion(project, session string, shown, answerable bool) {
	if project == "" || session == "" {
		return
	}
	_, _ = updateActiveCoderState(activeCoderPath(project, stateRoot()), func(state map[string]any) {
		evidence, ok := state["triggering_evidence"].(map[string]any)
		if !ok || evidence["question_session"] != session {
			return
		}
		if shown {
			evidence["question_shown"] = true
		}
		if answerable && evidence["question_shown"] == true {
			evidence["answerable"] = true
		}
	})
}

func markFallbackAnswerable(project, session string) {
	updateFallbackQuestion(project, session, false, true)
}

func promptRetryContext(path, session string) string {
	state := readAdvisorState(path, session)
	pending := state["codex_retry_pending"]
	state["codex_retry_count"], state["codex_retry_pending"] = 0, false
	_ = writeAdvisorState(path, state)
	if value, ok := pending.(string); ok && strings.TrimSpace(value) != "" {
		return "Previous turn ended with an unfinished Purser step: " + value
	}
	if pending == true {
		return "Resume the uncollected Purser job first by calling codex_result."
	}
	return ""
}

func compactionAdvice(event map[string]any, repository, statePath, session string) (result string) {
	defer func() {
		if recover() != nil {
			result = ""
		}
	}()
	settings := compactionSettings()
	records := readTranscriptRecords(stringValue(event["transcript_path"]))
	context := transcriptUsageContext(records)
	state := readAdvisorState(statePath, session)
	if !settings.enabled {
		return ""
	}
	if context < settings.threshold {
		state["reminder_messages"], state["reminder_wasted_tokens"] = 0, 0
		_ = writeAdvisorState(statePath, state)
	}
	receiptAdviceText := ""
	if receipt, ok := state["receipt_pending"].(map[string]any); ok {
		receiptAdviceText = receiptAdvice(receipt)
		state["receipt_pending"] = nil
		_ = writeAdvisorState(statePath, state)
	}
	if outcome, ok := state["compaction_outcome_pending"].(map[string]any); ok {
		adviceTurn, turnOK := exactInteger(outcome["advice_turn"])
		if turnOK && (outcome["compaction_observed"] == true || compactBoundaryAfterAdvice(records, number(state["context_prompt_count"]), adviceTurn)) {
			outcome["compaction_observed"] = true
			if post, exists := compactedContext(event["transcript_path"]); exists {
				outcome["post_context_tokens"] = post
			}
			count := number(state["context_prompt_count"])
			outcome["elapsed_turns"] = max(0, count-adviceTurn)
			emitCompactionOutcome(session, outcome)
			state["compaction_outcome_pending"] = nil
			state["last_suggested_context"], state["reminder_messages"], state["reminder_wasted_tokens"] = 0, 0, 0
			if notice, ok := state["notice_pending"].(map[string]any); ok && notice["notice_id"] == outcome["notice_id"] {
				state["notice_pending"] = nil
			}
			_ = writeAdvisorState(statePath, state)
		}
	}
	if pending, ok := state["notice_pending"].(map[string]any); ok && validNoticePending(pending) {
		return joinContext(compactionNoticeAdvice(pending, stringSlice(pending["triggers"])), receiptAdviceText)
	}
	triggers := compactionAdviceTriggers(records, settings.idleMinutes)
	currentTask, completedTasks := completedCodexTasks(records)
	latestTask := latestCodexTask(records)
	if currentTask != "" && completedTasks[currentTask] && currentTask != str(state["last_task_id"]) {
		triggers = append(triggers, "task_end")
	}
	if latestTask != "" {
		state["last_task_id"] = latestTask
	}
	if context < settings.threshold {
		state["last_suggested_context"] = 0
		_ = writeAdvisorState(statePath, state)
		return receiptAdviceText
	}
	last := number(state["last_suggested_context"])
	if last == 0 && len(triggers) == 0 {
		triggers = append(triggers, "threshold")
	}
	reminderDue := false
	if last != 0 {
		state["reminder_messages"] = number(state["reminder_messages"]) + 1
		state["reminder_wasted_tokens"] = number(state["reminder_wasted_tokens"]) + max(0, context-postCompactContextTokens)
		reminderDue = number(state["reminder_messages"]) >= 15
		if reminderDue {
			state["reminder_messages"] = 0
		}
	}
	reminder := func() string {
		if !reminderDue {
			return receiptAdviceText
		}
		pct := max(0, int(math.Round(100*(1-float64(postCompactContextTokens)/float64(context)))))
		notice := map[string]any{"context": context, "percentage": pct, "handoff": filepath.Join(filepath.Dir(statePath), "purser-handoff.md")}
		word := strings.Builder{}
		for _, r := range "Reminder" {
			word.WriteRune(r)
			word.WriteRune('\u0332')
		}
		return joinContext("Translate this reminder into the user's language, keeping the markdown exactly and the underline on every character of the translated word (markdown has no underline). Put it as the very last text of the turn after a `---` line:", fmt.Sprintf("> **%s: Tokens are leaking; not compacting has cost ~%dK tokens so far.**\n%s", word.String(), number(state["reminder_wasted_tokens"])/1000, compactionBlockquote(notice, true)), receiptAdviceText)
	}
	if last != 0 && len(triggers) != 0 && context < last+settings.regrow {
		_ = writeAdvisorState(statePath, state)
		return reminder()
	}
	if len(triggers) == 0 {
		_ = writeAdvisorState(statePath, state)
		return reminder()
	}
	if err := updateAdvisorHandoff(repository, records, context, triggers); err != nil {
		return receiptAdviceText
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return receiptAdviceText
	}
	pct := max(0, int(math.Round(100*(1-float64(postCompactContextTokens)/float64(context)))))
	notice := map[string]any{"context": context, "percentage": pct,
		"handoff":  filepath.Join(filepath.Dir(statePath), "purser-handoff.md"),
		"triggers": triggers, "blocked": false, "notice_id": hex.EncodeToString(id),
		"advice_written": false, "advice_turn": 0}
	state["last_suggested_context"], state["reminder_messages"], state["notice_pending"] = context, 0, notice
	if writeAdvisorState(statePath, state) != nil {
		return receiptAdviceText
	}
	return joinContext(compactionNoticeAdvice(notice, triggers), receiptAdviceText)
}

func recordAdviceWritten(path, session string) {
	defer func() { _ = recover() }()
	state := readAdvisorState(path, session)
	notice, ok := state["notice_pending"].(map[string]any)
	if !ok || notice["advice_written"] == true {
		return
	}
	id, _ := notice["notice_id"].(string)
	context, contextOK := exactInteger(notice["context"])
	if !contextOK || !noticeIDPattern.MatchString(id) {
		return
	}
	notice["advice_written"] = true
	notice["advice_turn"] = number(state["context_prompt_count"]) + 1
	outcome := map[string]any{"notice_id": id, "pre_context_tokens": context,
		"advice_turn": notice["advice_turn"], "compaction_observed": false}
	state["compaction_outcome_pending"] = outcome
	if writeAdvisorState(path, state) == nil {
		emitCompactionAdviceWritten(session, outcome)
	}
}

func compactionNoticeAdvice(notice map[string]any, triggers []string) string {
	context, _ := exactInteger(notice["context"])
	handoff := str(notice["handoff"])
	return fmt.Sprintf("Purser: %s context tokens; trigger(s): %s. Handoff: %s. After answering the current message, update its Goal, State, Decisions, Next, Open risks, Files to load sections (under 4,000 characters total). End each State and Decisions line with its source (commit or job). Keep the first Goal line `User language: <language>` set to the language the user writes in. Link by path to any existing repository handoff such as NEXT.md or HANDOFF.md. If your reply asks the user a question, also write that question under Next in the handoff so it survives compaction. End your turn with this as the very last text, after every tool call and everything else including any question: a line `---` and then this four-line blockquote translated into the user's language (keep the markdown exactly):\n%s\nThe percentage is an estimate; keep the ~. Do not compact or clear context yourself.",
		formatThousands(context), strings.Join(triggers, ", "), handoff, compactionBlockquote(notice, false))
}

func compactionBlockquote(notice map[string]any, bold bool) string {
	context, _ := exactInteger(notice["context"])
	percentage, _ := exactInteger(notice["percentage"])
	handoff := str(notice["handoff"])
	emphasis := "*"
	if bold {
		emphasis = "**"
	}
	return fmt.Sprintf("> ⚓ **Context passed %dK. Purser prepared your handoff.**\n> %sType `/compact` now to save **~%d%%** on every later message!%s\n> %sThe ship stays dry: decisions and the next step will be read from the handoff Purser prepared. Nothing is lost.%s\n> %sHandoff: `%s`%s", context/1000, emphasis, percentage, emphasis, emphasis, emphasis, emphasis, handoff, emphasis)
}

func formatThousands(value int) string {
	s := strconv.Itoa(value)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func transcriptUsageContext(records []map[string]any) int {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i]["subtype"] == "compact_boundary" {
			return 0
		}
		if transcriptRecordType(records[i]) != "assistant" {
			continue
		}
		message, _ := records[i]["message"].(map[string]any)
		if message == nil {
			message = records[i]
		}
		usage, _ := message["usage"].(map[string]any)
		total := 0
		for _, key := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
			if n, ok := exactInteger(usage[key]); ok && n >= 0 {
				total += n
			}
		}
		return total
	}
	return 0
}

func compactBoundaryAfterAdvice(records []map[string]any, promptCount, adviceTurn int) bool {
	boundary := -1
	for i, record := range records {
		if record["subtype"] == "compact_boundary" {
			boundary = i
		}
	}
	if boundary < 0 {
		return false
	}
	postBoundaryPrompts := 0
	for _, record := range records[boundary+1:] {
		if genuineTranscriptUser(record) {
			postBoundaryPrompts++
		}
	}
	return promptCount-postBoundaryPrompts >= adviceTurn
}

type advisorSettings struct {
	enabled                        bool
	threshold, idleMinutes, regrow int
}

func compactionSettings() advisorSettings {
	s := advisorSettings{true, 250000, 20, 60000}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(filepath.Join(home, ".orchestrator", "config.toml"))
	if err != nil {
		return s
	}
	section := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line == "[compaction]"
			continue
		}
		if !section {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		switch key {
		case "enabled":
			if value == "true" {
				s.enabled = true
			} else if value == "false" {
				s.enabled = false
			}
		case "threshold_tokens", "idle_minutes", "regrow_tokens":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				switch key {
				case "threshold_tokens":
					s.threshold = n
				case "idle_minutes":
					s.idleMinutes = n
				case "regrow_tokens":
					s.regrow = n
				}
			}
		}
	}
	return s
}

func compactionAdviceTriggers(records []map[string]any, idleMinutes int) []string {
	triggers, _ := compactionTranscriptFacts(records)
	if !containsString(triggers, "idle") {
		latestUser := -1
		for i, record := range records {
			if genuineTranscriptUser(record) {
				latestUser = i
			}
		}
		segment := records[latestUser+1:]
		for i := len(segment) - 1; i >= 0; i-- {
			if transcriptRecordType(segment[i]) != "assistant" {
				continue
			}
			if stamp, ok := transcriptTimestamp(segment[i]); ok && time.Since(stamp) >= time.Duration(idleMinutes)*time.Minute {
				triggers = append([]string{"idle"}, triggers...)
			}
			break
		}
	}
	return triggers
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func latestCodexTask(records []map[string]any) string {
	var latest string
	for _, record := range records {
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			name, _ := block["name"].(string)
			if name != "mcp__plugin_purser_codex-main__codex" && name != "mcp__plugin_purser_codex-main__codex-reply" {
				continue
			}
			input, _ := block["input"].(map[string]any)
			prompt := str(input["prompt"])
			if match := advisorTask.FindStringSubmatch(prompt); len(match) == 2 {
				latest = match[1]
			}
		}
	}
	return latest
}

func completedCodexTasks(records []map[string]any) (string, map[string]bool) {
	latestUser := -1
	for i, record := range records {
		if genuineTranscriptUser(record) {
			latestUser = i
		}
	}
	segment := records[latestUser+1:]
	type toolUse struct{ id, task string }
	var uses []toolUse
	results := map[string]int{}
	for i, record := range segment {
		for _, raw := range transcriptBlocks(record) {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "tool_use" {
				id := str(block["id"])
				name := str(block["name"])
				if id == "" || (name != "mcp__plugin_purser_codex-main__codex" && name != "mcp__plugin_purser_codex-main__codex-reply") {
					continue
				}
				input, _ := block["input"].(map[string]any)
				task := ""
				if match := advisorTask.FindStringSubmatch(str(input["prompt"])); len(match) == 2 {
					task = match[1]
				}
				uses = append(uses, toolUse{id, task})
			} else if block["type"] == "tool_result" && str(block["tool_use_id"]) != "" {
				results[str(block["tool_use_id"])] = i
			}
		}
	}
	latest := ""
	completed := map[string]bool{}
	for _, use := range uses {
		if use.task != "" {
			latest = use.task
		}
		result, ok := results[use.id]
		if !ok || use.task == "" {
			continue
		}
		for _, record := range segment[result+1:] {
			if transcriptRecordType(record) == "assistant" {
				completed[use.task] = true
				break
			}
		}
	}
	return latest, completed
}

func updateAdvisorHandoff(repository string, records []map[string]any, context int, triggers []string) error {
	path := filepath.Join(stateRoot(), "projects", repositoryProjectKey(repository), "telemetry", "purser-handoff.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		content = []byte("# Purser handoff\n## Goal\nUser language: <the language the user writes in>\n## State\n## Decisions\n## Next\n## Open risks\n## Files to load\n<!-- PURSER:STATE:START -->\n<!-- PURSER:STATE:END -->\n")
	}
	lines := []string{"as of " + time.Now().UTC().Format("2006-01-02T15:04Z"), fmt.Sprintf("Context tokens: %d [transcript]", context), fmt.Sprintf("Triggers: %s [transcript]", strings.Join(triggers, ", ")), "Last Codex calls:"}
	_, calls := compactionTranscriptFacts(records)
	for _, call := range calls[max(0, len(calls)-5):] {
		thread := call["threadId"]
		if thread == nil || thread == "" {
			thread = "unknown"
		}
		review := call["claude_review_mode"]
		if review == nil || review == "" {
			review = "unknown"
		}
		lines = append(lines, fmt.Sprintf("- task=%v tool=%v threadId=%v claude_review_mode=%v", call["task_id"], call["tool"], thread, review))
	}
	if plan := activePlanLine(repository); plan != "" {
		lines = append(lines, plan)
	}
	if output, e := runGit(repository, "log", "--oneline", "-5"); e == nil {
		lines = append(lines, "Last commits:")
		for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
			if line != "" && len(lines) < 12 {
				lines = append(lines, "- "+line)
			}
		}
	}
	if output, e := runGit(repository, "status", "--porcelain"); e == nil {
		count := 0
		for _, line := range strings.Split(output, "\n") {
			if strings.TrimSpace(line) != "" {
				count++
			}
		}
		lines = append(lines, fmt.Sprintf("Uncommitted changed paths: %d [git status]", count))
	}
	body := strings.Join(lines, "\n")
	if len([]rune(body)) > 1500 {
		runes := []rune(body)
		body = trimPythonWhitespace(string(runes[:1486]) + "\n... truncated")
	}
	text := string(content)
	start, end := strings.Index(text, stateStart), strings.Index(text, stateEnd)
	if start >= 0 && end > start {
		start += len(stateStart)
		text = text[:start] + "\n" + body + "\n" + text[end:]
	} else {
		text = strings.TrimRightFunc(text, isPythonWhitespace) + "\n" + stateStart + "\n" + body + "\n" + stateEnd + "\n"
	}
	text = replaceRecentTurns(text, formatRecentTurns(records))
	return writeHandoffAtomically(path, []byte(text))
}

func emitCompactionAdviceWritten(session string, outcome map[string]any) {
	pre, _ := exactInteger(outcome["pre_context_tokens"])
	if pre < 0 {
		return
	}
	payload := map[string]any{"notice_id": outcome["notice_id"], "advice_written": true, "compaction_observed": false, "pre_context_tokens": pre}
	eventID := newEventID()
	if eventID == "" {
		return
	}
	normalized := normalizeTelemetryID(session)
	stamp := time.Now().UTC()
	event := map[string]any{"schema_version": 1, "event_id": eventID, "ts_utc": stamp.Format(time.RFC3339Nano), "event_type": "COMPACTION_ADVICE_OUTCOME", "task_id": nil, "project_key": nil, "session_id": normalized, "repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil, "payload": payload}
	root := filepath.Join(stateRoot(), "telemetry", "events")
	if os.MkdirAll(root, 0700) != nil {
		return
	}
	lock, err := os.OpenFile(filepath.Join(root, ".append.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer lock.Close()
	if info, _ := lock.Stat(); info != nil && info.Size() == 0 {
		_, _ = lock.WriteAt([]byte("0"), 0)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	if tryLock(lock) != nil {
		_ = writeJSONAtomically(filepath.Join(root, "spool", eventID+".json"), event)
		return
	}
	defer unlock(lock)
	path := filepath.Join(root, stamp.Format("2006-01"), "session_"+normalized+".jsonl")
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(data, '\n'))
	_ = file.Sync()
	_ = file.Close()
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
