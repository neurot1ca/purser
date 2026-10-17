package enforce

import (
	"fmt"
	"sort"
	"strings"

	"purser-hook/internal/ledger"
	"purser-hook/internal/profile"
)

type postProfile struct {
	Stored                 bool                        `json:"stored"`
	ProtectedPaths         []string                    `json:"protected_paths"`
	ForceFullReview        bool                        `json:"force_full_review"`
	RequireDifferentFamily bool                        `json:"require_different_family,omitempty"`
	SkipMaxLines           *int                        `json:"skip_max_lines"`
	SkipMaxFiles           *int                        `json:"skip_max_files"`
	ReviewerExecutable     any                         `json:"reviewer_executable"`
	Reviewer               any                         `json:"reviewer"`
	ReviewerCandidates     []profile.ReviewerCandidate `json:"reviewer_candidates"`
	AuthoritySources       any                         `json:"authority_sources"`
	Configured             *bool                       `json:"configured"`
	UserStatus             any                         `json:"user_status"`
	ProjectStatus          any                         `json:"project_status"`
	FirstWarning           any                         `json:"first_warning"`
}

func postProfileFromPending(pending, loadedConfig map[string]any) postProfile {
	paths, pathsOK := postStringList(pending["protected_paths"])
	forceFull, forceOK := pending["force_full_review"].(bool)
	requireDifferentFamily, _ := pending["require_different_family"].(bool)
	lines, linesOK := pythonInt(pending["skip_max_lines"])
	files, filesOK := pythonInt(pending["skip_max_files"])
	configured, configuredOK := pending["configured"].(bool)
	userStatus, userOK := pending["user_status"].(string)
	projectStatus, projectOK := pending["project_status"].(string)
	candidates, candidatesOK := postReviewerCandidates(pending["reviewer_candidates"])
	stored := pathsOK && forceOK && linesOK && filesOK && configuredOK && userOK && projectOK && candidatesOK
	if !stored {
		model, _ := loadedConfig["reviewer1_model_requested"].(string)
		return postProfile{
			ProtectedPaths: profile.EngineProtectedPaths(), ForceFullReview: true,
			Reviewer: "agy", ReviewerCandidates: []profile.ReviewerCandidate{{Agent: "agy", Model: model}},
		}
	}
	if len(candidates) == 0 {
		model, _ := loadedConfig["reviewer1_model_requested"].(string)
		candidates = []profile.ReviewerCandidate{{Agent: "agy", Model: model}}
	}
	reviewer, exists := pending["reviewer"]
	if !exists {
		reviewer = "agy"
	}
	return postProfile{
		Stored: true, ProtectedPaths: paths, ForceFullReview: forceFull, RequireDifferentFamily: requireDifferentFamily,
		SkipMaxLines: &lines, SkipMaxFiles: &files,
		ReviewerExecutable: pending["reviewer_executable"], Reviewer: reviewer,
		ReviewerCandidates: candidates, AuthoritySources: pending["authority_sources"],
		Configured: &configured, UserStatus: userStatus, ProjectStatus: projectStatus,
		FirstWarning: pending["first_warning"],
	}
}

func postReviewerCandidates(value any) ([]profile.ReviewerCandidate, bool) {
	if value == nil {
		return nil, true
	}
	items, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]profile.ReviewerCandidate); typedOK {
			return append([]profile.ReviewerCandidate{}, typed...), true
		}
		return nil, false
	}
	result := make([]profile.ReviewerCandidate, 0, len(items))
	for _, item := range items {
		candidate, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		agent, agentOK := candidate["agent"].(string)
		model, modelOK := candidate["model"].(string)
		if !agentOK || !modelOK {
			return nil, false
		}
		effort, _ := candidate["effort"].(string)
		result = append(result, profile.ReviewerCandidate{Agent: agent, Model: model, Effort: effort})
	}
	return result, true
}

func pythonInt(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), true
	case float64:
		return int(number), number == float64(int(number))
	default:
		return 0, false
	}
}
func applyOutOfScopeResult(bundle map[string]any, result map[string]any, protectedPaths []string) bool {
	findings, ok := bundle["findings"].([]any)
	if !ok {
		findings = []any{}
	}
	status, _ := result["status"].(string)
	if status != "OK" {
		code, exists := result["code"]
		if !exists {
			code = "OUT_OF_SCOPE_CHECK_FAILED"
		}
		message, exists := result["message"]
		if !exists {
			message = "Out-of-scope modification check failed; forcing FULL Claude review."
		}
		bundle["findings"] = append(findings, map[string]any{"code": code, "message": message})
		return true
	}
	paths, ok := postStringList(result["paths"])
	if !ok || len(paths) == 0 {
		return false
	}
	message := fmt.Sprintf("%d file(s) modified outside declared scope: %s", len(paths), strings.Join(paths[:min(5, len(paths))], ", "))
	bundle["findings"] = append(findings, map[string]any{"code": "OUT_OF_SCOPE_MODIFICATION", "message": message})
	existing, _ := postStringList(bundle["protected_paths_touched"])
	merged := map[string]bool{}
	for _, path := range existing {
		merged[path] = true
	}
	for _, path := range ledger.DetectProtectedPaths(paths, &protectedPaths) {
		merged[path] = true
	}
	protected := make([]string, 0, len(merged))
	for path := range merged {
		protected = append(protected, path)
	}
	sort.Strings(protected)
	bundle["protected_paths_touched"] = protected
	return true
}

func postStringList(value any) ([]string, bool) {
	switch items := value.(type) {
	case []string:
		return append([]string{}, items...), true
	case []any:
		result := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			result = append(result, text)
		}
		return result, true
	default:
		return nil, false
	}
}
