package jobs

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
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
