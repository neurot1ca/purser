package enforce

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"purser-hook/internal/contextio"
	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

type postReviewerAttempt struct {
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// runPostReviewer selects candidates in Python's configured order and invokes
// the shared reviewer bridge under one deadline. It does not interpret a
// completed review; that belongs to the following migration pass.
func runPostReviewer(root, stateRoot string, pending, bundle map[string]any, resolved postProfile, loaded map[string]any, delta, classification map[string]any, started time.Time, hookBudget time.Duration) (review.BridgeOutcome, error) {
	candidates := resolved.ReviewerCandidates
	if len(candidates) == 0 {
		candidates = []profile.ReviewerCandidate{
			{Agent: "agy", Model: stringValue(loaded["reviewer1_model_requested"])},
			{Agent: "claude", Model: "claude-haiku-5-5"},
			{Agent: "codex", Model: "gpt-6-luna", Effort: "low"},
		}
	}
	implementer := vendorFamily(stringValue(pending["fallback_family"]))
	modelsPresent := false
	if models, ok := pending["attempted_models"].([]any); ok {
		modelsPresent = len(models) > 0
	} else if models, ok := pending["attempted_models"].([]string); ok {
		modelsPresent = len(models) > 0
	}
	if implementer == "" && modelsPresent {
		implementer = vendorFamily(modelFamily(stringValue(pending["model"])))
	}
	if implementer == "" {
		implementer = vendorFamily(stringValue(loaded["coder_family"]))
		if implementer == "" {
			implementer = vendorFamily(modelFamily(stringValue(pending["model"])))
		}
	}
	configuredSeconds := 90.0
	if n, ok := loaded["reviewer1_timeout_seconds"].(float64); ok {
		configuredSeconds = n
	} else if n, ok := loaded["reviewer1_timeout_seconds"].(int); ok {
		configuredSeconds = float64(n)
	} else if n, ok := loaded["reviewer1_timeout_seconds"].(json.Number); ok {
		if parsed, err := n.Float64(); err == nil {
			configuredSeconds = parsed
		}
	}
	remaining := time.Until(started.Add(hookBudget))
	timeout := time.Duration(configuredSeconds * float64(time.Second))
	if remaining < timeout {
		timeout = remaining
	}
	deadline := time.Now().Add(timeout)
	repoRoot := stringValue(pending["repo_root"])
	if worktree := stringValue(pending["worktree_path"]); worktree != "" {
		repoRoot = worktree
	}
	projectKey := stringValue(pending["project_key"])
	if !reviewerProjectKey.MatchString(projectKey) && repoRoot != "" {
		projectKey = contextio.ProjectKey(repoRoot)
	}
	circuitDir := ""
	if stateRoot != "" && projectKey != "" {
		circuitDir = filepath.Join(stateRoot, "projects", projectKey, "telemetry")
	}
	args := map[string]any{
		"task_id": stringValue(pending["task_id"]), "phase": stringValue(pending["phase"]),
		"route": stringValue(pending["route"]), "model": stringValue(pending["model"]),
		"effort": stringValue(pending["effort"]), "repo_root": repoRoot,
		"delta": delta, "classification": classification,
	}
	for key, value := range map[string]any{
		"thread_id": bundle["thread_id"], "project_key": pending["project_key"], "state_root": stateRoot,
		"task_objective": pending["task_objective"], "acceptance_criteria": pending["acceptance_criteria"],
		"validation_summary": reviewerValidationSummary(bundle), "reviewer_executable": resolved.ReviewerExecutable,
		"authority_sources": resolved.AuthoritySources,
	} {
		if value != nil && (key != "task_objective" && key != "acceptance_criteria" || stringValue(value) != "") &&
			(key != "reviewer_executable" || stringValue(value) != "") {
			args[key] = value
		}
	}
	attempts := []postReviewerAttempt{}
	var last review.BridgeOutcome
	invoked := false
	for index, candidate := range candidates {
		vendor := vendorFamily(modelFamily(candidate.Model))
		if implementer != "" && vendor != "" && implementer == vendor {
			attempts = append(attempts, postReviewerAttempt{Agent: candidate.Agent, Model: candidate.Model, Outcome: "SKIPPED_SAME_VENDOR"})
			continue
		}
		if !postReviewerAdapters[candidate.Agent] {
			attempts = append(attempts, postReviewerAttempt{Agent: candidate.Agent, Model: candidate.Model, Outcome: "SKIPPED_ADAPTER_UNREGISTERED"})
			continue
		}
		if circuitDir != "" && time.Until(deadline) > 0 && hasEligibleReviewer(candidates[index+1:], implementer) &&
			review.ReviewerCircuitOpen(circuitDir, candidate.Agent, candidate.Model, time.Now()) {
			attempts = append(attempts, postReviewerAttempt{Agent: candidate.Agent, Model: candidate.Model, Outcome: "UNAVAILABLE", Reason: "REVIEWER_CIRCUIT_OPEN"})
			continue
		}
		budget := time.Until(deadline)
		if budget <= 0 {
			attempts = append(attempts, postReviewerAttempt{Agent: candidate.Agent, Model: candidate.Model, Outcome: "UNAVAILABLE_BUDGET_EXHAUSTED"})
			continue
		}
		for _, later := range candidates[index+1:] {
			laterVendor := vendorFamily(modelFamily(later.Model))
			if postReviewerAdapters[later.Agent] && (implementer == "" || laterVendor == "" || implementer != laterVendor) {
				budget = budget * 60 / 100
				break
			}
		}
		candidateArgs := clonePostMap(args)
		candidateArgs["reviewer_agent"], candidateArgs["reviewer_model"] = candidate.Agent, candidate.Model
		if candidate.Agent == "claude" {
			effort := candidate.Effort
			if !reviewerEfforts[effort] {
				effort = stringValue(loaded["reviewer1_effort"])
			}
			if !reviewerEfforts[effort] {
				effort = "medium"
			}
			candidateArgs["reviewer_effort"] = effort
		} else if candidate.Effort != "" {
			candidateArgs["reviewer_effort"] = candidate.Effort
		}
		candidateArgs["reviewer_timeout_seconds"] = budget.Seconds()
		bridgeResult, err := postReviewerBridge(context.Background(), root, candidateArgs, budget)
		if err != nil {
			if review.IsReviewerBridgeException(err) {
				bundle["findings"] = appendPostFinding(bundle["findings"], "INTERNAL_POST_ERROR", "Reviewer-1 bridge raised unexpectedly.")
			}
			return review.BridgeOutcome{}, err
		}
		last = bridgeResult
		invoked = true
		outcome := "COMPLETED"
		if last.Outcome == "unavailable" {
			outcome = "UNAVAILABLE"
		}
		attempt := postReviewerAttempt{Agent: candidate.Agent, Model: candidate.Model, Outcome: outcome}
		if outcome == "UNAVAILABLE" {
			attempt.Reason = postReviewerReason(last.Attributes)
		}
		attempts = append(attempts, attempt)
		if circuitDir != "" {
			review.RecordReviewerCircuitOutcome(circuitDir, candidate.Agent, candidate.Model, stringValue(last.Attributes["error_code"]), time.Now())
		}
		if outcome != "UNAVAILABLE" {
			break
		}
	}
	if len(attempts) > 1 {
		bundle["reviewer1_attempts"] = attempts
	}
	if !invoked {
		lastAttempt := postReviewerAttempt{Agent: "agy", Model: stringValue(loaded["reviewer1_model_requested"])}
		if len(attempts) > 0 {
			lastAttempt = attempts[len(attempts)-1]
		}
		reason := "REVIEWER1_UNAVAILABLE"
		if lastAttempt.Outcome == "SKIPPED_ADAPTER_UNREGISTERED" {
			reason = "REVIEWER_ADAPTER_UNAVAILABLE"
		} else if lastAttempt.Outcome == "SKIPPED_SAME_VENDOR" {
			reason = "REVIEWER_VENDOR_CONFLICT"
		}
		bundle["reviewer1_skipped_reason"] = reason
		last = review.BridgeOutcome{Outcome: "unavailable", Attributes: map[string]any{
			"review_status": "UNAVAILABLE", "provider": lastAttempt.Agent,
			"reviewer1_model_requested": lastAttempt.Model, "result": "REVIEWER1_UNAVAILABLE",
			"validation": "UNKNOWN", "findings": []any{map[string]any{"code": reason,
				"message": "No eligible registered Reviewer-1 adapter was available; FULL review required."}},
		}}
	}
	return last, nil
}

func hasEligibleReviewer(candidates []profile.ReviewerCandidate, implementer string) bool {
	for _, candidate := range candidates {
		vendor := vendorFamily(modelFamily(candidate.Model))
		if postReviewerAdapters[candidate.Agent] && (implementer == "" || vendor == "" || implementer != vendor) {
			return true
		}
	}
	return false
}

var reviewerEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
var reviewerProjectKey = regexp.MustCompile(`^[0-9a-f]{16}$`)

func postReviewerReason(attributes map[string]any) string {
	code := stringValue(attributes["error_code"])
	message := ""
	switch findings := attributes["findings"].(type) {
	case []any:
		if len(findings) > 0 {
			if finding, ok := findings[0].(map[string]any); ok {
				message, code = stringValue(finding["message"]), firstNonempty(code, stringValue(finding["code"]))
			} else if finding, ok := findings[0].(map[string]string); ok {
				message, code = finding["message"], firstNonempty(code, finding["code"])
			}
		}
	case []map[string]string:
		if len(findings) > 0 {
			message, code = findings[0]["message"], firstNonempty(code, findings[0]["code"])
		}
	}
	message = prefixRunes(message, 200)
	if code == "" {
		code = "REVIEWER1_UNAVAILABLE"
	}
	if message == "" {
		return code
	}
	return code + ": " + message
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var postReviewerAdapters = map[string]bool{"agy": true, "opencode": true, "codex": true, "claude": true}

var postReviewerBridge = review.RunBridge

var modelVendorPrefix = regexp.MustCompile(`^(openai|google|deepseek|anthropic|qwen|mistral|llama)/`)

func modelFamily(model string) string {
	name := strings.ToLower(model)
	if match := modelVendorPrefix.FindStringSubmatch(name); len(match) == 2 {
		if match[1] == "anthropic" {
			return "claude"
		}
		return match[1]
	}
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	switch {
	case strings.HasPrefix(name, "gemini-"):
		return "gemini"
	case strings.HasPrefix(name, "gpt-") || regexp.MustCompile(`^o[134](-|$)`).MatchString(name):
		return "gpt"
	case strings.HasPrefix(name, "claude-"):
		return "claude"
	case name == "haiku" || name == "sonnet" || name == "opus":
		return "claude"
	case strings.HasPrefix(name, "deepseek-"):
		return "deepseek"
	case strings.HasPrefix(name, "qwen-") || regexp.MustCompile(`^qwen[0-9]`).MatchString(name):
		return "qwen"
	case strings.HasPrefix(name, "mistral-") || regexp.MustCompile(`^mistral[0-9]`).MatchString(name):
		return "mistral"
	case strings.HasPrefix(name, "llama-") || regexp.MustCompile(`^llama[0-9]`).MatchString(name):
		return "llama"
	default:
		return ""
	}
}

func vendorFamily(family string) string {
	normalized := strings.ToLower(strings.TrimSpace(family))
	if normalized == "" {
		return ""
	}
	if i := strings.IndexAny(normalized, "-/"); i >= 0 {
		normalized = normalized[:i]
	}
	switch normalized {
	case "gpt", "openai":
		return "openai"
	case "gemini", "google":
		return "google"
	case "claude", "anthropic":
		return "anthropic"
	case "qwen3":
		return "qwen"
	case "llama3":
		return "llama"
	default:
		for _, known := range []string{"qwen", "mistral", "llama"} {
			suffix := strings.TrimPrefix(normalized, known)
			if suffix != normalized && strings.Trim(suffix, "0123456789") == "" {
				return known
			}
		}
		return normalized
	}
}

func reviewerValidationSummary(bundle map[string]any) string {
	worker := mapValue(bundle["worker_validation"])
	status, summary := stringValue(worker["status"]), stringValue(worker["summary"])
	if utf8.RuneCountInString(summary) > 1200 {
		runes := []rune(summary)
		summary = string(runes[:1200])
	}
	return fmt.Sprintf("Worker-reported validation summary (status=%s): %s", status, summary)
}

func clonePostMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+3)
	for key, value := range source {
		result[key] = value
	}
	return result
}
