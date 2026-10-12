package jobs

import (
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var responsivenessOffsets = []float64{0, 30, 60, 120, 240}
var retryHeader = regexp.MustCompile(`(?i)\bretry[- ]after\s*[:=]?\s*(\d+(?:\.\d+)?)\s*(?:seconds?|s)?\b`)
var creditError = regexp.MustCompile(`(?i)\b(?:HTTP\s*)?402\b|enforced_spend_limit_reached|insufficient (?:account )?balance|quota[- ]exceeded|billing.{0,40}(?:quota|credit)|(?:quota|credit).{0,40}billing`)
var rateError = regexp.MustCompile(`(?i)\b429\b|rate[- ]limit(?:ed)?`)

func RetryAfterSeconds(value any) *float64 {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if strings.EqualFold(strings.ReplaceAll(key, "_", "-"), "retry-after") {
				var s string
				switch n := item.(type) {
				case string:
					s = n
				case float64:
					s = strconv.FormatFloat(n, 'f', -1, 64)
				case int:
					s = strconv.Itoa(n)
				}
				if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
					return &n
				}
			}
			if n := RetryAfterSeconds(item); n != nil {
				return n
			}
		}
	case []any:
		for _, item := range v {
			if n := RetryAfterSeconds(item); n != nil {
				return n
			}
		}
	case []map[string]any:
		for _, item := range v {
			if n := RetryAfterSeconds(item); n != nil {
				return n
			}
		}
	}
	return nil
}

// LaunchFailureEvidence classifies a launch and proves retry safety only for empty pre-work failures.
func LaunchFailureEvidence(agent string, events []map[string]any, stderr string, started, stdout, output bool, code *int) map[string]any {
	evidence := func(kind string, after any, proof bool) map[string]any {
		return map[string]any{"type": kind, "retryAfterSeconds": after, "noWorkProof": proof}
	}
	if !started {
		return map[string]any{"type": "AMBIGUOUS", "retryAfterSeconds": nil, "noWorkProof": false, "proof": "PROCESS_NOT_STARTED"}
	}
	var body strings.Builder
	body.WriteString(stderr)
	work := false
	for _, e := range events {
		b, _ := json.Marshal(e)
		body.Write(b)
		body.WriteByte(' ')
		kind, _ := e["type"].(string)
		switch agent {
		case "codex":
			work = work || strings.HasPrefix(kind, "item.")
		case "opencode":
			part, _ := e["part"].(map[string]any)
			tool, _ := part["tool"].(string)
			work = work || kind == "tool_use" || kind == "text" || strings.Contains(strings.ToLower(kind), "edit") || tool == "edit" || tool == "write" || tool == "patch"
		default:
			work = work || kind != "error"
		}
	}
	text := body.String()
	after := RetryAfterSeconds(events)
	if after == nil {
		if m := retryHeader.FindStringSubmatch(text); len(m) > 1 {
			n, _ := strconv.ParseFloat(m[1], 64)
			after = &n
		}
	}
	if !work && creditError.MatchString(text) {
		return evidence("CREDIT_EXHAUSTED", nil, false)
	}
	if rateError.MatchString(text) {
		return evidence("RATE_LIMITED", after, !work && !stdout && !output)
	}
	if agent == "opencode" && !work {
		for _, e := range events {
			er, _ := e["error"].(map[string]any)
			name, _ := er["name"].(string)
			if e["type"] == "error" && (name == "ProviderError" || name == "ModelNotFoundError") {
				return evidence("PROVIDER_ERROR", nil, true)
			}
		}
		if code != nil && *code != 0 {
			return evidence("NONZERO_EXIT", nil, true)
		}
	}
	if len(events) == 0 && !stdout && !output && strings.TrimSpace(stderr) == "" {
		e := evidence("NO_RESPONSE", nil, true)
		e["proof"] = "NO_EVENTS_OR_OUTPUT"
		return e
	}
	return evidence("AMBIGUOUS", nil, false)
}

// reserveCapacityRetry consumes the durable budget before waiting or starting
// another worker. The current host keeps the launch intent and generation.
func reserveCapacityRetry(path, id string, generation int64, token string, evidence map[string]any) (time.Duration, bool, error) {
	if evidence["type"] != "RATE_LIMITED" || evidence["noWorkProof"] != true {
		return 0, false, nil
	}
	lock, err := AcquireOwnership(filepath.Dir(path), id)
	if err != nil {
		return 0, false, err
	}
	defer lock.Release()
	job, err := Read(path)
	if err != nil {
		return 0, false, err
	}
	actual, valid := recoveryInteger(job, "generation")
	var owner, state string
	job.Get("ownershipToken", &owner)
	job.Get("state", &state)
	attempts := int64(0)
	if _, exists := job.values["capacityRetryAttempts"]; exists {
		var ok bool
		attempts, ok = recoveryInteger(job, "capacityRetryAttempts")
		valid = valid && ok
	}
	if !valid || actual != generation || owner != token || state != "RUNNING" || attempts < 0 || attempts >= 2 {
		return 0, false, nil
	}
	delay := time.Duration(1<<attempts) * time.Second
	if after, ok := evidence["retryAfterSeconds"].(*float64); ok && after != nil {
		if math.IsNaN(*after) || math.IsInf(*after, 0) || *after < 0 {
			return 0, false, nil
		}
		delay = max(delay, time.Duration(math.Min(*after, 30)*float64(time.Second)))
	}
	deadline, valid := numericDeadline(job)
	if !valid || delay >= time.Until(time.Unix(0, int64(deadline*1e9))) {
		return 0, false, nil
	}
	if err = job.Set("capacityRetryAttempts", attempts+1); err != nil {
		return 0, false, err
	}
	job.Delete("exitCode")
	if err = Write(path, job); err != nil {
		return 0, false, err
	}
	return delay, true, nil
}

func RunWithResponsivenessRetries(run func() map[string]any, clock func() time.Duration, sleep func(time.Duration)) map[string]any {
	start := clock()
	var result map[string]any
	for i, offset := range responsivenessOffsets {
		delay := time.Duration(offset*float64(time.Second)) - (clock() - start)
		if delay > 0 {
			sleep(delay)
		}
		result = run()
		ev, _ := result["typedEvidence"].(map[string]any)
		if ev["type"] != "NO_RESPONSE" || ev["noWorkProof"] != true {
			result["responsivenessAttempts"] = i + 1
			return result
		}
	}
	result["responsivenessAttempts"] = len(responsivenessOffsets)
	return result
}

type RetryInput struct {
	Agent, Model, FallbackAgent, FallbackModel, FallbackEffort string
	Mechanical, Async                                          bool
}
type Attempt func(agent, model string) map[string]any

// RunMechanicalFallback retries only proven no-work failures on an unchanged tree.
func RunMechanicalFallback(in RetryInput, run Attempt, candidates func() []string, markFailed func(string) bool, unchanged func() bool) map[string]any {
	call := func() map[string]any { return run(in.Agent, in.Model) }
	annotate := func(r map[string]any, served bool) {
		r["attemptedModels"] = []string{in.Model}
		r["servedAgent"] = nil
		r["servedModel"] = nil
		if served {
			r["servedAgent"] = in.Agent
			r["servedModel"] = in.Model
		}
	}
	if !in.Mechanical {
		return call()
	}
	if !in.Async {
		r := call()
		if v, ok := r["structuredContent"].(map[string]any); ok {
			annotate(v, true)
		}
		return r
	}
	if in.Agent != "opencode" || (in.FallbackAgent != "codex" && in.FallbackAgent != "opencode" && in.FallbackAgent != "agy") {
		return call()
	}
	attempted := []string{in.Model}
	fallbackUsed := false
	for {
		r := call()
		if r["isError"] != true {
			if v, ok := r["structuredContent"].(map[string]any); ok {
				v["attemptedModels"] = attempted
				v["servedAgent"] = in.Agent
				v["servedModel"] = in.Model
			}
			return r
		}
		stop := func() map[string]any {
			r["attemptedModels"] = attempted
			r["servedAgent"] = nil
			r["servedModel"] = nil
			return r
		}
		if in.Agent != "opencode" || fallbackUsed {
			return stop()
		}
		ev, _ := r["typedEvidence"].(map[string]any)
		missing := r["retryableModelMissing"] == true
		if (!missing && ev["noWorkProof"] != true) || unchanged == nil || !unchanged() {
			return stop()
		}
		if missing {
			if markFailed == nil || !markFailed(in.Model) {
				return stop()
			}
			for _, m := range candidates() {
				seen := false
				for _, a := range attempted {
					if a == m {
						seen = true
						break
					}
				}
				if !seen {
					in.Agent, in.Model = "opencode", m
					attempted = append(attempted, m)
					goto retry
				}
			}
			if in.FallbackAgent != "codex" {
				in.Agent, in.Model = in.FallbackAgent, in.FallbackModel
				fallbackUsed = true
				attempted = append(attempted, in.Model)
				goto retry
			}
		} else if in.FallbackAgent != "codex" {
			return stop()
		}
		in.Agent, in.Model = "codex", in.FallbackModel
		fallbackUsed = true
		attempted = append(attempted, in.Model)
	retry:
	}
}
