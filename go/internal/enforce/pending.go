package enforce

import (
	"encoding/json"
	"strings"

	"purser-hook/internal/ledger"
	"purser-hook/internal/profile"
)

// PendingCallInput contains values already selected by Pre's parse, route,
// coder, contract, and profile steps. BuildPendingCall does not persist them.
type PendingCallInput struct {
	TaskID, ProjectKey, Phase, Route, Model, Effort, Label, Agent string
	FallbackFamily, ProjectRootReason, PriorThreadID              *string
	Scope                                                         []string
	Prompt                                                        string
	TaskObjective, AcceptanceCriteria                             string
	TaskContractGrounded                                          bool
	TaskType                                                      string
	SuspectedRouteMismatch                                        any
	HasCodeMap                                                    bool
	Reason, ReasonSource                                          *string
	RouteAdjustmentReason                                         *string
	ETAMinutes                                                    any
	PreviousRoute, NewRoute                                       *string
	ReplyCall, Queued                                             bool
	WorktreeReplyPath                                             *string
	Repository                                                    *string
	ManifestBudgetSeconds                                         float64
	LiveThread                                                    map[string]any
	Profile                                                       *profile.ResolvedProfile
	ReadinessCheck                                                any
	MechanicalOpenCodeEnabled                                     *bool
}

// BuildPendingCall mirrors Python Pre's baseline/handoff and synchronous
// pending record construction. Lease admission remains the caller's concern.
func BuildPendingCall(in PendingCallInput) (map[string]any, map[string]any) {
	var baseline map[string]any
	if in.Repository == nil {
		baseline = map[string]any{"status": "UNKNOWN", "code": "PROJECT_ROOT_UNRESOLVED", "message": optionalString(in.ProjectRootReason), "baseline_head": nil}
	} else {
		baselineRoot := *in.Repository
		if in.WorktreeReplyPath != nil {
			baselineRoot = *in.WorktreeReplyPath
		}
		var scope []string
		if in.Phase != "plan" && len(in.Scope) > 0 {
			scope = in.Scope
		}
		baseline = ledger.CaptureBaseline(baselineRoot, scope, in.ManifestBudgetSeconds)
	}
	if in.ReplyCall && baseline["status"] == "OK" && handoffMatches(baseline, in.LiveThread, in.Scope) {
		baseline = in.LiveThread["origin_baseline"].(map[string]any)
	}
	scope := in.Scope
	if scope == nil {
		scope = []string{}
	}
	mechanicalEnabled := in.MechanicalOpenCodeEnabled == nil || *in.MechanicalOpenCodeEnabled
	mechanical := in.Route == "mechanical" && in.Phase == "implementation" && mechanicalEnabled
	pending := map[string]any{
		"kind": "call", "task_id": in.TaskID, "project_key": in.ProjectKey,
		"phase": in.Phase, "route": in.Route, "model": in.Model, "effort": in.Effort,
		"label": in.Label, "agent": in.Agent, "fallback_family": optionalString(in.FallbackFamily),
		"mechanical_opencode": mechanical, "shadow_prompt": nil, "scope": scope,
		"repo_root": optionalString(in.Repository), "project_root_reason": optionalString(in.ProjectRootReason),
		"baseline": baseline, "prior_thread_id": optionalString(in.PriorThreadID),
		"task_objective": in.TaskObjective, "acceptance_criteria": in.AcceptanceCriteria,
		"task_contract_grounded": in.TaskContractGrounded, "readiness_check": in.ReadinessCheck,
		"task_type": in.TaskType, "suspected_route_mismatch": in.SuspectedRouteMismatch,
		"has_code_map": in.HasCodeMap, "reason": optionalString(in.Reason), "eta_minutes": in.ETAMinutes,
		"reason_source": optionalString(in.ReasonSource), "previous_route": optionalString(in.PreviousRoute), "new_route": optionalString(in.NewRoute),
		"route_adjustment_reason": optionalString(in.RouteAdjustmentReason),
	}
	if mechanical {
		pending["shadow_prompt"] = in.Prompt
	}
	if in.WorktreeReplyPath != nil {
		pending["worktree_path"] = *in.WorktreeReplyPath
	}
	if in.Profile != nil {
		p := in.Profile
		checks := any(checkSnapshots(p.Checks))
		if in.ReplyCall && in.LiveThread != nil {
			if inherited, ok := in.LiveThread["profile_checks"].([]any); ok {
				checks = inherited
			}
		}
		var firstWarning any
		if len(p.Warnings) > 0 {
			firstWarning = p.Warnings[0]
		}
		candidates := make([]map[string]any, 0, len(p.ReviewerCandidates))
		for _, candidate := range p.ReviewerCandidates {
			serialized := map[string]any{"agent": candidate.Agent, "model": candidate.Model}
			if candidate.Effort != "" {
				serialized["effort"] = candidate.Effort
			}
			candidates = append(candidates, serialized)
		}
		sources := make([]map[string]any, 0, len(p.AuthoritySources))
		for _, source := range p.AuthoritySources {
			sources = append(sources, map[string]any{"path": source.Path, "headings": source.Headings, "when_changed": source.WhenChanged})
		}
		pending["profile_checks"], pending["protected_paths"] = checks, p.ProtectedPaths
		pending["force_full_review"], pending["skip_max_lines"] = p.ForceFullReview, p.SkipMaxLines
		pending["skip_max_files"], pending["reviewer_executable"] = p.SkipMaxFiles, p.ReviewerExecutable
		pending["reviewer"], pending["reviewer_candidates"] = p.Roles.Reviewer, candidates
		pending["authority_sources"], pending["vault_root"] = sources, p.VaultRoot
		pending["configured"], pending["user_status"] = p.Configured, p.UserStatus
		pending["project_status"], pending["first_warning"] = p.ProjectStatus, firstWarning
	}
	if in.Queued {
		queued := map[string]any{"status": "UNKNOWN", "code": "QUEUED"}
		pending["lease_queued"], pending["baseline"] = true, queued
		baseline = queued
	}
	return baseline, pending
}

func optionalString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func handoffMatches(captured map[string]any, live map[string]any, scope []string) bool {
	if live == nil || captured["status"] != "OK" {
		return false
	}
	origin, ok := live["origin_baseline"].(map[string]any)
	if !ok || origin["status"] != "OK" || !sameStringList(live["origin_scope"], scope) {
		return false
	}
	post, ok := live["post_state"].(map[string]any)
	return ok && post["status"] == "OK" && jsonEqual(baselineStateFields(captured), baselineStateFields(post))
}

func sameStringList(value any, expected []string) bool {
	var actual []string
	switch values := value.(type) {
	case []string:
		actual = values
	case []any:
		actual = make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return false
			}
			actual = append(actual, text)
		}
	default:
		return false
	}
	if actual == nil {
		actual = []string{}
	}
	if expected == nil {
		expected = []string{}
	}
	return jsonEqual(actual, expected)
}

func jsonEqual(left, right any) bool {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false
	}
	var normalizedLeft, normalizedRight any
	if json.Unmarshal(leftJSON, &normalizedLeft) != nil || json.Unmarshal(rightJSON, &normalizedRight) != nil {
		return false
	}
	return pythonJSONEqual(normalizedLeft, normalizedRight)
}

func pythonJSONEqual(left, right any) bool {
	switch leftValue := left.(type) {
	case nil:
		return right == nil
	case bool:
		switch rightValue := right.(type) {
		case bool:
			return leftValue == rightValue
		case float64:
			return rightValue == 0 && !leftValue || rightValue == 1 && leftValue
		}
	case float64:
		switch rightValue := right.(type) {
		case float64:
			return leftValue == rightValue
		case bool:
			return leftValue == 0 && !rightValue || leftValue == 1 && rightValue
		}
	case string:
		rightValue, ok := right.(string)
		return ok && leftValue == rightValue
	case []any:
		rightValue, ok := right.([]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for index := range leftValue {
			if !pythonJSONEqual(leftValue[index], rightValue[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		rightValue, ok := right.(map[string]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for key, value := range leftValue {
			other, exists := rightValue[key]
			if !exists || !pythonJSONEqual(value, other) {
				return false
			}
		}
		return true
	}
	return false
}

func baselineStateFields(value map[string]any) map[string]any {
	fields := make(map[string]any, len(value))
	for key, item := range value {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "timestamp") && !strings.Contains(lower, "duration") && !strings.Contains(lower, "elapsed") && !strings.Contains(lower, "budget") {
			fields[key] = item
		}
	}
	return fields
}

func checkSnapshots(checks []profile.Check) []map[string]any {
	result := make([]map[string]any, 0, len(checks))
	for _, check := range checks {
		var when any
		if check.When != nil {
			when = check.When
		}
		result = append(result, map[string]any{"name": check.Name, "argv": check.Argv, "when": when, "timeout_seconds": check.TimeoutSeconds})
	}
	return result
}
