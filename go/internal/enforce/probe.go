package enforce

import (
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"strings"
)

// ProbeCommand builds the Codex API probe invocation without starting a process.
func ProbeCommand(prefix []string, cwd, model, outputPath string) []string {
	command := append(append([]string{}, prefix...), "exec", "--json", "--skip-git-repo-check")
	if model != "" {
		command = append(command, "-m", model)
	}
	command = append(command, "-s", "read-only", "-C", cwd, "-o", filepath.Clean(outputPath), "-")
	return command
}

// ProbeEvent returns Codex's terminal/failure/text interpretation for one envelope.
func ProbeEvent(event map[string]any) (bool, string, bool) {
	kind, _ := event["type"].(string)
	failure := ""
	if kind == "error" || kind == "turn.failed" {
		failure = probeEventMessage(event)
	}
	final := false
	if kind == "item.completed" {
		item, _ := event["item"].(map[string]any)
		final = item["type"] == "agent_message"
	}
	return kind == "turn.completed" || kind == "turn.failed" || kind == "error", failure, final
}

func probeEventMessage(event map[string]any) string {
	parts := []string{}
	for _, key := range []string{"message", "error"} {
		switch value := event[key].(type) {
		case string:
			parts = append(parts, value)
		case map[string]any:
			for _, field := range []string{"message", "code"} {
				if item, ok := value[field].(string); ok {
					parts = append(parts, item)
				}
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, ": ")
	}
	data, _ := json.Marshal(event)
	return string(data)
}

var probeRetryAfter = regexp.MustCompile(`(?i)\bretry[- ]after\s*[:=]?\s*(\d+(?:\.\d+)?)\s*(?:seconds?|s)?\b`)
var probeCredit = regexp.MustCompile(`(?i)\b(?:HTTP\s*)?402\b|enforced_spend_limit_reached|insufficient (?:account )?balance|quota[- ]exceeded|billing.{0,40}(?:quota|credit)|(?:quota|credit).{0,40}billing`)
var probeRate = regexp.MustCompile(`(?i)\b429\b|rate[- ]limit(?:ed)?`)

func probeRetryValue(value any) (float64, bool) {
	switch item := value.(type) {
	case map[string]any:
		for key, nested := range item {
			if strings.EqualFold(strings.ReplaceAll(key, "_", "-"), "retry-after") {
				var seconds float64
				switch raw := nested.(type) {
				case float64:
					seconds = raw
				case string:
					if parsed, err := json.Number(raw).Float64(); err == nil {
						seconds = parsed
					} else {
						continue
					}
				default:
					continue
				}
				if !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 {
					return seconds, true
				}
			}
			if seconds, ok := probeRetryValue(nested); ok {
				return seconds, true
			}
		}
	case []any:
		for _, nested := range item {
			if seconds, ok := probeRetryValue(nested); ok {
				return seconds, true
			}
		}
	case []map[string]any:
		for _, nested := range item {
			if seconds, ok := probeRetryValue(nested); ok {
				return seconds, true
			}
		}
	}
	return 0, false
}

func probeEvidence(kind string, retry any, noWork bool, proof string) map[string]any {
	result := map[string]any{"type": kind, "retryAfterSeconds": retry, "noWorkProof": noWork}
	if proof != "" {
		result["proof"] = proof
	}
	return result
}

// ProbeResult parses fake Codex JSONL and classifies the result. It performs no I/O.
func ProbeResult(stdout []byte, stderr string, outputHasData, processStarted bool, returncode int, timedOut bool) map[string]any {
	events := []map[string]any{}
	for _, line := range strings.Split(string(stdout), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimSuffix(line, "\r")), &event) == nil && event != nil {
			events = append(events, event)
		}
	}
	if timedOut {
		return map[string]any{"ok": false, "failure": probeEvidence("NO_RESPONSE", nil, false, "")}
	}
	completed, failures := false, false
	for _, event := range events {
		terminal, failure, _ := ProbeEvent(event)
		if event["type"] == "turn.completed" {
			completed = true
		}
		if failure != "" {
			failures = true
		}
		_ = terminal
	}
	if returncode == 0 && completed && !failures {
		return map[string]any{"ok": true}
	}
	return map[string]any{"ok": false, "failure": ProbeFailure(stdout, stderr, events, outputHasData, processStarted)}
}

// ProbeFailure is the Codex-specific launch evidence classifier.
func ProbeFailure(stdout []byte, stderr string, events []map[string]any, outputHasData, processStarted bool) map[string]any {
	if !processStarted {
		return probeEvidence("AMBIGUOUS", nil, false, "PROCESS_NOT_STARTED")
	}
	body := stderr
	for _, event := range events {
		data, _ := json.Marshal(event)
		body += "\n" + string(data)
	}
	var retry any
	if seconds, ok := probeRetryValue(events); ok {
		retry = seconds
	} else if match := probeRetryAfter.FindStringSubmatch(body); len(match) > 1 {
		if seconds, err := json.Number(match[1]).Float64(); err == nil {
			retry = seconds
		}
	}
	workStarted := false
	for _, event := range events {
		if kind, _ := event["type"].(string); strings.HasPrefix(kind, "item.") {
			workStarted = true
			break
		}
	}
	if !workStarted && probeCredit.MatchString(body) {
		return probeEvidence("CREDIT_EXHAUSTED", nil, false, "")
	}
	if !workStarted && probeRate.MatchString(body) {
		return probeEvidence("RATE_LIMITED", retry, false, "")
	}
	if len(events) == 0 && len(strings.TrimSpace(string(stdout))) == 0 && !outputHasData && strings.TrimSpace(stderr) == "" {
		return probeEvidence("NO_RESPONSE", nil, true, "NO_EVENTS_OR_OUTPUT")
	}
	return probeEvidence("AMBIGUOUS", nil, false, "")
}
