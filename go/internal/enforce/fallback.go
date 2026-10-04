package enforce

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/policy"
)

// fallbackPreDecision mirrors the fallback and low-quota entry in codex.py Pre.
// State persistence and question delivery remain owned by contextio.
type fallbackPreDecision struct {
	Action   string `json:"action"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Question string `json:"question"`
	Kind     string `json:"kind"`
}

func decideFallbackPre(parsed policy.OrchParseResult, session string, active map[string]any,
	fallback map[string]any, quota, launchFailure map[string]any, now time.Time) fallbackPreDecision {
	evidence, _ := active["triggering_evidence"].(map[string]any)
	consent := evidence != nil && hasKey(evidence, "question_session")
	model := ""
	if fallback != nil {
		model, _ = fallback["model"].(string)
	}

	if fallback != nil && active["active_coder"] == "primary" {
		if reason(parsed) == "user_override" {
			if !consent || evidence["question_session"] != session || evidence["question_shown"] != true || evidence["answerable"] != true {
				return denyFallback("FALLBACK_CONSENT_REQUIRED", "reason=user_override requires a shown fallback question and a later user prompt.")
			}
			return fallbackPreDecision{Action: "approve", Kind: "consent"}
		}
		if consent && evidence["question_session"] == session && evidence["answerable"] == true {
			return fallbackPreDecision{Action: "decline", Kind: "decline"}
		}
		if consent {
			return denyFallback("FALLBACK_CONSENT_REQUIRED", "Wait for the fallback question to be shown and answered before dispatch.")
		}
		var trigger map[string]any
		if parsed.Phase != nil && *parsed.Phase == "implementation" && quotaNumber(quota["5h_used_pct"]) >= 95 && stringValue(quota["5h_resets_at"]) != "" {
			trigger = map[string]any{"type": "QUOTA", "trigger_id": quota["5h_resets_at"]}
		}
		if trigger == nil {
			trigger = launchFailure
		}
		declined := evidence != nil && evidence["declined"] == true && evidence["trigger_id"] == triggerValue(trigger, "trigger_id")
		if parsed.Phase != nil && *parsed.Phase == "implementation" && trigger != nil && !declined && session != "" {
			kind, question := fallbackQuestion(trigger, model, quota, now)
			return fallbackPreDecision{Action: "question", Code: "FALLBACK_CONSENT_REQUIRED", Message: "A fallback question must be shown before another implementation dispatch.", Question: question, Kind: kind}
		}
	}
	if quota != nil && (active == nil || active["active_coder"] == "primary") && reason(parsed) != "user_override" {
		reset := quota["5h_resets_at"]
		declinedQuota := evidence != nil && evidence["declined"] == true && evidence["type"] == "QUOTA" && evidence["trigger_id"] == reset
		var windows []string
		for _, label := range []string{"5h", "7d"} {
			used := quotaNumber(quota[label+"_used_pct"])
			rst, ok := quota[label+"_resets_at"].(string)
			if ok && used >= 90 && !(label == "5h" && declinedQuota) {
				windows = append(windows, fmt.Sprintf("%s: %g%% used, resets at %s", label, used, rst))
			}
		}
		if len(windows) > 0 {
			message := "Codex quota is nearly exhausted (" + strings.Join(windows, "; ") + "). Tell the user before this task starts; the user may switch the coding agent or wait. If the user chooses to proceed with Codex, resend the same call with reason=user_override in the ORCH line."
			if resetAt, ok := reset.(string); ok {
				message += " If the user chooses to wait, run a background sleep command until the 5h reset at " + resetAt + " UTC, then resend the call when it exits; waiting costs nothing."
			}
			return denyFallback("LOW_CODEX_QUOTA", message)
		}
	}
	return fallbackPreDecision{Action: "continue"}
}

// decideFallbackReturn mirrors the Python fallback return-to-primary branch.
func decideFallbackReturn(active map[string]any, session, reason string, quota map[string]any, now time.Time) string {
	evidence, _ := active["triggering_evidence"].(map[string]any)
	if active["active_coder"] != "fallback" || evidence == nil {
		return "continue"
	}
	if hasKey(evidence, "question_session") {
		if evidence["question_session"] != session || evidence["question_shown"] != true || evidence["answerable"] != true {
			return "deny"
		}
		if reason == "user_override" {
			return "accept"
		}
		return "decline"
	}
	switch evidence["type"] {
	case "QUOTA":
		marker, markerOK := evidence["reset_cycle_marker"].(string)
		if marker == "" {
			marker, markerOK = active["reset_cycle_marker"].(string)
		}
		reset, _ := quota["5h_resets_at"].(string)
		markerAt, markerErr := time.Parse(time.RFC3339Nano, marker)
		resetAt, resetErr := time.Parse(time.RFC3339Nano, reset)
		if markerOK && markerErr == nil && resetErr == nil && resetAt.After(markerAt) &&
			evidence["declined_reset_cycle_marker"] != reset && session != "" {
			return "offer"
		}
	case "CREDIT_EXHAUSTED", "NO_RESPONSE":
		interval := float64(15 * 60)
		if evidence["type"] == "NO_RESPONSE" {
			interval = 5 * 60
		}
		last := interval
		if value, ok := active["last_probe_time"].(string); ok {
			if stamp, err := strconv.ParseFloat(value, 64); err == nil {
				last = float64(now.UnixNano())/1e9 - stamp
			} else {
				last = 0
			}
		}
		if last >= interval {
			return "claim_probe"
		}
	case "RATE_LIMITED":
		delay, ok := fallbackNumber(evidence["retryAfterSeconds"])
		if !ok || delay < 0 {
			delay = 5 * 60
		}
		started, err := strconv.ParseFloat(stringValue(evidence["switched_at"]), 64)
		if err == nil && float64(now.UnixNano())/1e9 >= started+delay {
			return "cooldown_return"
		}
	}
	return "continue"
}

func fallbackQuestion(trigger map[string]any, model string, quota map[string]any, now time.Time) (string, string) {
	switch triggerValue(trigger, "type") {
	case "QUOTA":
		minutes := 5
		if reset, ok := quota["5h_resets_at"].(string); ok {
			if stamp, err := time.Parse(time.RFC3339Nano, reset); err == nil {
				minutes = max(1, int(math.Ceil(stamp.Sub(now).Minutes())))
			}
		}
		return "quota", fmt.Sprintf("> ⚓ **Your quota is nearly exhausted, so the next task may be interrupted. Would you like to wait for it to renew (%d minutes) or switch to the fallback coder (%s)?**", minutes, model)
	case "CREDIT_EXHAUSTED":
		return "credit", fmt.Sprintf("> ⚓ **Your API credits are exhausted, so the next task can't start. Would you like to add credits or switch to the fallback coder (%s)?**", model)
	case "RATE_LIMITED":
		minutes := 5
		if seconds, ok := fallbackNumber(trigger["retryAfterSeconds"]); ok {
			minutes = max(1, int(math.Ceil(seconds/60)))
		}
		return "rate_limit", fmt.Sprintf("> ⚓ **You've hit the API rate limit. Would you like to wait for it to reset (%d minutes) or switch to the fallback coder (%s)?**", minutes, model)
	default:
		return "unresponsive", fmt.Sprintf("> ⚓ **Codex did not respond after 5 attempts. Would you like to retry later or switch to the fallback coder (%s)?**", model)
	}
}

func denyFallback(code, message string) fallbackPreDecision {
	return fallbackPreDecision{Action: "deny", Code: code, Message: code + ": " + message}
}
func reason(parsed policy.OrchParseResult) string {
	if parsed.Reason != nil {
		return *parsed.Reason
	}
	return ""
}

func hasKey(value map[string]any, key string) bool { _, ok := value[key]; return ok }
func triggerValue(value map[string]any, key string) any {
	if value == nil {
		return nil
	}
	return value[key]
}
func quotaNumber(value any) float64 {
	number, ok := fallbackNumber(value)
	if !ok {
		return -1
	}
	return number
}

func fallbackNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := strconv.ParseFloat(string(number), 64)
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}
