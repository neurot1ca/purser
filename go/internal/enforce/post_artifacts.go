package enforce

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const planOutputLimit = 15000

var planSummaryHeading = regexp.MustCompile(`(?im)^#{0,6}\s*Summary:\s*(.*)$`)
var planNextHeading = regexp.MustCompile(`(?m)^#{1,6}\s+\S`)

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
	runes := []rune(report)
	if len(runes) > planOutputLimit {
		bundle["findings"] = appendFinding(bundle["findings"], "PLAN_OUTPUT_TOO_LONG", "Plan exceeds 15,000 characters.")
		runes = runes[:planOutputLimit]
	}
	content := string(runes)
	if strings.TrimSpace(report) == "" {
		bundle["findings"] = appendFinding(bundle["findings"], "PLAN_OUTPUT_EMPTY", "Plan output is empty.")
	}
	if err := atomicWrite(path, []byte(content)); err != nil {
		return "", err
	}
	bundle["plan_path"], bundle["plan_summary"] = path, postPlanSummary(content)
	return path, nil
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
		return prefixRunes(report, 4000)
	}
	tail := report[match[2]:]
	if len(tail) > 0 {
		if next := planNextHeading.FindStringIndex(tail[1:]); next != nil {
			tail = tail[:next[0]+1]
		}
	}
	return prefixRunes(strings.TrimSpace(tail), 4000)
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
	default:
		return fallback
	}
}

// finalizePostArtifacts adds the quota snapshot within the compact-response byte cap.
// The complete bundle is persisted by finalizeBundle before its response is compacted.
func finalizePostArtifacts(root string, event, bundle map[string]any, cap, reportLimit int) (map[string]any, string, error) {
	compact, path, err := finalizeBundle(root, event, bundle, cap, reportLimit)
	if err != nil {
		return nil, "", err
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
				if report, ok := compact["implementer_report"].(string); ok {
					compact["implementer_report"] = prefixRunes(report, reportLimit)
				}
			} else {
				delete(compact, "codex_quota")
			}
		}
	}
	return compact, path, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
