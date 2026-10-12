package enforce

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var planSummaryHeading = regexp.MustCompile(`(?im)^#{0,6}\s*Summary:\s*(.*)$`)
var planNextHeading = regexp.MustCompile(`(?m)^(#{1,6})\s+\S`)

func storePostPlan(root string, event map[string]any, taskID any, report string, bundle map[string]any) (string, error) {
	name := fmt.Sprint(taskID)
	if taskID == nil || name == "" || name == "<nil>" || taskID == false || taskID == 0 || taskID == float64(0) {
		name = "task"
	}
	name = regexp.MustCompile(`[^A-Za-z0-9_-]`).ReplaceAllString(name, "_")
	if utf8.RuneCountInString(name) > 80 {
		name = string([]rune(name)[:80])
	}
	key, err := callKey(event)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(root, "plans", name+"-"+key+".md"))
	if err != nil {
		return "", err
	}
	content := report
	if strings.TrimSpace(report) == "" {
		bundle["findings"] = appendFinding(bundle["findings"], "PLAN_OUTPUT_EMPTY", "Plan output is empty.")
	}
	missing := make([]string, 0, 7)
	for _, heading := range []string{"Summary:", "Goal and boundaries", "Current evidence", "Decision", "Milestones", "Risks/open decisions", "Implementation handoff"} {
		if !planHasHeading(report, heading) {
			missing = append(missing, heading)
		}
	}
	if len(missing) > 0 {
		bundle["findings"] = appendFinding(bundle["findings"], "PLAN_SHAPE_MISSING", "Plan lacks the required sections: "+strings.Join(missing, ", ")+".")
	}
	if err := atomicWrite(path, []byte(content)); err != nil {
		return "", err
	}
	summary := postPlanSummary(content)
	if summary == "" && strings.TrimSpace(content) != "" {
		summary = "Plan summary is empty; complete plan is available at " + path + "."
	}
	bundle["plan_path"], bundle["plan_summary"] = path, summary
	return path, nil
}

func planHasHeading(report, heading string) bool {
	pattern := regexp.MustCompile(`(?im)^#{0,6}\s*` + regexp.QuoteMeta(heading) + `(?:\s|:|$)`)
	return pattern.MatchString(report)
}

func appendFinding(value any, code, message string) []any {
	findings, ok := value.([]any)
	if !ok {
		findings = []any{}
	}
	return append(findings, map[string]any{"code": code, "message": message})
}

func postPlanSummary(report string) string {
	match := planSummaryHeading.FindStringSubmatchIndex(report)
	if match == nil {
		return report
	}
	tail := report[match[2]:]
	level := len(strings.TrimSpace(report[match[0]:match[1]])) - len(strings.TrimLeft(strings.TrimSpace(report[match[0]:match[1]]), "#"))
	for _, next := range planNextHeading.FindAllStringSubmatchIndex(tail, -1) {
		nextLevel := next[3] - next[2]
		if level == 0 || nextLevel <= level {
			tail = tail[:next[0]]
			break
		}
	}
	return strings.TrimSpace(tail)
}

func prefixRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}

func postReportLimit(config map[string]any, phase any) int {
	key, fallback := "implementation_report_max_chars", 1200
	if phase == "operation" {
		key, fallback = "operation_report_max_chars", 24000
	}
	switch value := config[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case json.Number:
		if parsed, err := value.Float64(); err == nil {
			return int(parsed)
		}
	default:
		return fallback
	}
	return fallback
}

// finalizePostArtifacts adds the quota snapshot within the compact-response byte cap.
// The complete bundle is persisted by finalizeBundle before its response is compacted.
func finalizePostArtifacts(root string, event, bundle map[string]any, cap, reportLimit int) (map[string]any, string, error) {
	// Python persists the original bundle, before response-only derived fields
	// and validation-line compaction are applied.
	artifactBundle := make(map[string]any, len(bundle))
	for key, value := range bundle {
		artifactBundle[key] = value
	}
	delete(artifactBundle, "delta")
	delete(artifactBundle, "force_full_review")
	artifactBundle["changed_path_count"] = nil
	_, path, err := finalizeBundle(root, event, artifactBundle, cap, reportLimit)
	if err != nil {
		return nil, "", err
	}
	bundle["full_bundle_path"] = artifactBundle["full_bundle_path"]
	// finalizeBundle may have promoted the bundle when its mandatory fields
	// exceeded the response cap. Keep that decision in the caller's bundle.
	bundle["claude_review_mode"] = artifactBundle["claude_review_mode"]
	bundle["audit_receipt"] = artifactBundle["audit_receipt"]
	if metadata, ok := artifactBundle["review_navigation"]; ok {
		bundle["review_navigation"] = metadata
	}
	delete(bundle, "_review_diff")
	compact, _, err := compactBundle(bundle, cap)
	if err != nil {
		return nil, "", err
	}
	delete(compact, "delta")
	delete(compact, "force_full_review")
	stripWorkerValidation(compact)
	if report, ok := compact["implementer_report"].(string); ok {
		compact["implementer_report"] = prefixRunes(report, reportLimit)
	}
	if quota, exists := bundle["codex_quota"]; exists {
		compact["codex_quota"] = quota
		size, sizeErr := nonReportBytes(compact)
		if sizeErr != nil {
			return nil, "", sizeErr
		}
		if excess := size - cap; excess > 0 {
			smaller, overflow, compactErr := compactBundle(bundle, maxInt(0, cap-excess))
			if compactErr != nil {
				return nil, "", compactErr
			}
			smaller["codex_quota"] = quota
			if size, compactErr = nonReportBytes(smaller); compactErr != nil {
				return nil, "", compactErr
			} else if !overflow && size <= cap {
				compact = smaller
				stripWorkerValidation(compact)
				if report, ok := compact["implementer_report"].(string); ok {
					compact["implementer_report"] = prefixRunes(report, reportLimit)
				}
			} else {
				delete(compact, "codex_quota")
			}
		}
	}
	applyReviewNavigationPreview(compact, bundle)
	return compact, path, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
