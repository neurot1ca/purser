package jobs

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EventResult is the interpretation of an agent's JSON event stream.
type EventResult struct {
	ThreadID string         `json:"threadId,omitempty"`
	Status   string         `json:"status"`
	Failure  string         `json:"failure,omitempty"`
	Final    string         `json:"final,omitempty"`
	Notes    []string       `json:"notes,omitempty"`
	Usage    map[string]any `json:"usage,omitempty"`
}

// EventStream consumes output as it arrives so ThreadID is available before
// the child exits, including when a later event fails.
type EventStream struct {
	agent   string
	pending []byte
	result  EventResult
	text    []string
	events  []map[string]any
}

func NewEventStream(agent string) *EventStream {
	return &EventStream{agent: agent, result: EventResult{Status: "incomplete"}}
}

// Write accepts arbitrary stdout chunks. A final unterminated JSON line is
// consumed by Result, matching the bridge's EOF behavior.
func (s *EventStream) Write(chunk []byte) string {
	s.pending = append(s.pending, chunk...)
	for {
		i := bytes.IndexByte(s.pending, '\n')
		if i < 0 {
			return s.result.ThreadID
		}
		s.consume(s.pending[:i])
		s.pending = append(s.pending[:0], s.pending[i+1:]...)
	}
}

func (s *EventStream) consume(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var event map[string]any
	if json.Unmarshal(line, &event) != nil || event == nil {
		return
	}
	kind, _ := event["type"].(string)
	s.events = append(s.events, event)
	session := ""
	var failure string
	terminal, success := false, false
	switch s.agent {
	case "agy":
		session = nonemptyString(event["conversation_id"])
		status, _ := event["status"].(string)
		response, responseOK := event["response"].(string)
		success = strings.EqualFold(status, "success") && responseOK
		terminal = status != ""
		if success {
			s.text = append(s.text, response)
		} else {
			failure = firstString(event["error"], status, "Antigravity completion missing")
		}
	case "opencode":
		session = nonemptyString(event["sessionID"])
		if kind == "error" {
			failure = eventMessage(event)
		}
		if kind == "tool_use" {
			part, _ := event["part"].(map[string]any)
			state, _ := part["state"].(map[string]any)
			if state["status"] == "error" {
				s.result.Notes = append(s.result.Notes, openCodeToolNote(part, state))
			}
		}
		part, _ := event["part"].(map[string]any)
		if kind == "text" {
			if value, ok := part["text"].(string); ok {
				s.text = append(s.text, value)
			}
		}
		terminal = kind == "step_finish" && part["reason"] == "stop" || failure != ""
		success = kind == "step_finish" && part["reason"] == "stop" && failure == ""
	case "codex":
		if kind == "thread.started" {
			session = nonemptyString(event["thread_id"])
		}
		if kind == "error" || kind == "turn.failed" {
			failure = eventMessage(event)
		}
		item, _ := event["item"].(map[string]any)
		if kind == "item.completed" && item["type"] == "agent_message" {
			if value, ok := item["text"].(string); ok {
				s.text = append(s.text, value)
			}
		}
		terminal = kind == "turn.completed" || kind == "turn.failed" || kind == "error"
		success = kind == "turn.completed" && failure == ""
		if kind == "turn.completed" {
			if usage, ok := event["usage"].(map[string]any); ok {
				s.result.Usage = codexUsage(usage)
			}
		}
	case "claude":
		if kind == "result" {
			session, _ = event["session_id"].(string)
			message, _ := event["result"].(string)
			if message != "" {
				s.text = append(s.text, message)
			}
			terminal = true
			success = event["is_error"] != true && event["subtype"] == "success"
			if !success {
				failure = firstString(event["result"], "Claude completion failed")
			}
		}
	}
	if s.result.ThreadID == "" && session != "" {
		s.result.ThreadID = session
	}
	if failure != "" {
		if s.result.Failure != "" {
			s.result.Failure += "\n" + failure
		} else {
			s.result.Failure = failure
		}
		s.result.Status = "error"
	} else if terminal && success && s.result.Failure == "" {
		s.result.Status = "success"
	}
}

func (s *EventStream) Events() []map[string]any { return s.events }

type testEvidenceRun struct {
	index int
	value map[string]any
}

func collectTestEvidence(events []map[string]any) ([]testEvidenceRun, bool) {
	latest := make(map[string]testEvidenceRun)
	lastEdit, lastRun := -1, -1
	for i, event := range events {
		if event["type"] != "item.completed" {
			continue
		}
		item, _ := event["item"].(map[string]any)
		if item["type"] == "file_change" && item["status"] == "completed" {
			lastEdit = i
		}
		if item["type"] != "command_execution" {
			continue
		}
		command, _ := item["command"].(string)
		if !testRunnerCommand(command) {
			continue
		}
		exitCode, ok := item["exit_code"].(float64)
		if !ok || exitCode != float64(int(exitCode)) {
			continue
		}
		lastRun = i
		output, _ := item["aggregated_output"].(string)
		lines := strings.Split(output, "\n")
		if len(lines) > 40 {
			lines = lines[len(lines)-40:]
		}
		tail := strings.Join(lines, "\n")
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
			for !utf8.ValidString(tail) {
				tail = tail[1:]
			}
		}
		latest[testRunnerTarget(command)] = testEvidenceRun{index: i, value: map[string]any{
			"command": command, "exit_code": int(exitCode), "output_tail": tail,
		}}
	}
	runs := make([]testEvidenceRun, 0, len(latest))
	failed := false
	for _, item := range latest {
		// Post-edit checks supersede pre-fix runs, regardless of command or tool.
		// Without a later check, retain the earlier evidence conservatively.
		if lastEdit >= 0 && lastRun > lastEdit && item.index < lastEdit {
			continue
		}
		runs = append(runs, item)
		if item.value["exit_code"].(int) != 0 {
			failed = true
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].index < runs[j].index })
	return runs, failed
}

// TestEvidence returns the latest completed test-runner command for each
// distinct command string after the last completed file change when checks
// follow that change, keeping only the six most recent runs.
func TestEvidence(events []map[string]any) []map[string]any {
	runs, _ := collectTestEvidence(events)
	if len(runs) > 6 {
		runs = runs[len(runs)-6:]
	}
	out := make([]map[string]any, len(runs))
	for i, item := range runs {
		out[i] = item.value
	}
	return out
}

// TestEvidenceFailed reports whether any latest-per-command test run failed,
// including runs omitted from TestEvidence's six-entry output cap.
func TestEvidenceFailed(events []map[string]any) bool {
	_, failed := collectTestEvidence(events)
	return failed
}

func testRunnerCommand(command string) bool {
	return testRunnerTarget(command) != ""
}

func testRunnerTarget(command string) string {
	words := shellWords(command)
	if len(words) == 0 {
		return ""
	}
	base := func(path string) string {
		path = strings.ToLower(strings.TrimSuffix(path, ".exe"))
		if i := strings.LastIndexAny(path, `/\\`); i >= 0 {
			path = path[i+1:]
		}
		return path
	}
	inner := command
	if shell := base(words[0]); shell == "pwsh" || shell == "powershell" || shell == "bash" || shell == "sh" || shell == "zsh" || shell == "cmd" {
		for i := 1; i < len(words)-1; i++ {
			flag := strings.ToLower(words[i])
			if shell != "cmd" && (flag == "-command" || flag == "-c" || flag == "-lc") || shell == "cmd" && flag == "/c" {
				inner = words[i+1]
				break
			}
		}
	}
	for _, segment := range shellSeparator.Split(inner, -1) {
		tokens := shellWords(segment)
		for len(tokens) > 0 {
			if strings.HasPrefix(tokens[0], "$env:") && len(tokens) >= 3 && tokens[1] == "=" {
				tokens = tokens[3:]
				continue
			}
			if strings.Contains(tokens[0], "=") && !strings.HasPrefix(tokens[0], "=") {
				tokens = tokens[1:]
				continue
			}
			break
		}
		if len(tokens) == 0 {
			continue
		}
		cmd := base(tokens[0])
		if cmd == "go" && len(tokens) > 1 && tokens[1] == "test" ||
			cmd == "pytest" || cmd == "uv" && len(tokens) > 2 && tokens[1] == "run" && base(tokens[2]) == "pytest" ||
			(cmd == "npm" || cmd == "pnpm" || cmd == "yarn" || cmd == "cargo") && len(tokens) > 1 && (tokens[1] == "test") ||
			(cmd == "python" || cmd == "python3" || cmd == "py") && len(tokens) > 2 && tokens[1] == "-m" && tokens[2] == "pytest" {
			return strings.Join(tokens, " ")
		}
	}
	return ""
}

func shellWords(command string) []string {
	var words []string
	var word strings.Builder
	var quote rune
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range command {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
		} else if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			flush()
		} else {
			word.WriteRune(r)
		}
	}
	flush()
	return words
}

var shellSeparator = regexp.MustCompile(`;|&&|\|\||\n`)

// Result flushes an unterminated final line and returns a snapshot.
func (s *EventStream) Result(interrupted bool, exitCode int, stderr string) EventResult {
	if len(bytes.TrimSpace(s.pending)) > 0 {
		s.consume(s.pending)
		s.pending = nil
	}
	if interrupted {
		s.result.Status = "interrupted"
		s.result.Failure = "cancelled"
	}
	if !interrupted && exitCode != 0 && s.result.Status != "error" {
		s.result.Status = "error"
		s.result.Failure = stderrTail(stderr)
		if s.result.Failure == "" {
			s.result.Failure = "exit code " + strconv.Itoa(exitCode)
		}
	}
	s.result.Final = strings.Join(s.text, "")
	if s.agent == "codex" && len(s.text) > 0 {
		s.result.Final = s.text[len(s.text)-1]
	}
	if interrupted {
		s.result.Final = ""
		s.result.Usage = nil
	}
	return s.result
}

func nonemptyString(value any) string {
	s, _ := value.(string)
	if s == "" {
		return ""
	}
	return s
}

func firstString(value any, fallback string, rest ...string) string {
	if s, ok := value.(string); ok && s != "" {
		return s
	}
	if fallback != "" {
		return fallback
	}
	for _, s := range rest {
		if s != "" {
			return s
		}
	}
	return ""
}

func eventMessage(event map[string]any) string {
	parts := make([]string, 0, 4)
	for _, value := range []any{event["message"], event["error"]} {
		switch item := value.(type) {
		case string:
			parts = append(parts, item)
		case map[string]any:
			for _, key := range []string{"message", "code"} {
				if s, ok := item[key].(string); ok {
					parts = append(parts, s)
				}
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, ": ")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func openCodeToolNote(part, state map[string]any) string {
	tool := nonemptyString(part["tool"])
	if tool == "" {
		tool = "unknown"
	}
	error := ""
	switch value := state["error"].(type) {
	case string:
		error = value
	case map[string]any:
		error = eventMessage(map[string]any{"error": value})
	}
	if error == "" {
		error = "OpenCode tool failed"
	}
	return "OpenCode tool error: " + tool + ": " + error
}

func codexUsage(value any) map[string]any {
	usage, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	allowed := map[string]bool{"input_tokens": true, "cached_input_tokens": true, "output_tokens": true, "reasoning_output_tokens": true}
	out := make(map[string]any)
	for key, value := range usage {
		if !allowed[key] {
			continue
		}
		n, ok := value.(float64)
		if ok && !math.IsInf(n, 0) && !math.IsNaN(n) && n >= 0 {
			out[key] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stderrTail(stderr string) string {
	runes := []rune(stderr)
	if len(runes) > 4000 {
		runes = runes[len(runes)-4000:]
	}
	return string(runes)
}
