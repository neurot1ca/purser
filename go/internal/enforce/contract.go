package enforce

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	contractBullet    = regexp.MustCompile(`^([-*+]|\d+[.)])\s+`)
	contractStop      = regexp.MustCompile(`^(task objective|objective|goal|acceptance criteria|acceptance|requirements|required changes?|done when|definition of done|tests?|validation|verified context|report):`)
	contractCriteria  = regexp.MustCompile(`^(acceptance criteria|acceptance|requirements|required changes?|done when|definition of done):`)
	contractHead      = regexp.MustCompile(`^(#{1,3})[ \t]+(.+?)\s*$`)
	contractCount     = regexp.MustCompile(`\b(count|counting|number of|how many)\b`)
	contractRunbook   = regexp.MustCompile(`\b(established|documented) runbook\b`)
	contractOperation = regexp.MustCompile(`\b(deploy|runner control|fetch|package|packaging)\b`)
)

type contractSection struct {
	kind  string
	level int
	lines []string
}

// parseTaskContract extracts the same explicit contract forms as Python's oracle.
func parseTaskContract(prompt, taskID, phase string, scope []string) (string, string, bool) {
	defaultObjective := fmt.Sprintf("Task %s (%s phase) bounded to declared scope.", taskID, phase)
	defaultCriteria := fmt.Sprintf("Verify correct implementation within declared scope (%s) with no material regression or scope expansion.", strings.Join(scope, ", "))
	lines := strings.Split(prompt, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < len("TASK_CONTRACT:") || !strings.EqualFold(line[:len("TASK_CONTRACT:")], "TASK_CONTRACT:") {
			continue
		}
		var data map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line[len("TASK_CONTRACT:"):])), &data) != nil || data == nil {
			continue
		}
		objective, _ := data["objective"].(string)
		criteria := ""
		switch value := data["criteria"].(type) {
		case string:
			criteria = strings.TrimSpace(value)
		case []any:
			valid := true
			parts := make([]string, 0, len(value))
			for _, item := range value {
				text, ok := item.(string)
				if !ok {
					valid = false
					break
				}
				if text = strings.TrimSpace(text); text != "" {
					parts = append(parts, text)
				}
			}
			if valid {
				criteria = strings.Join(parts, "; ")
			}
		}
		if objective, criteria = strings.TrimSpace(objective), strings.TrimSpace(criteria); objective != "" && criteria != "" {
			return objective, criteria, true
		}
	}

	var objective string
	criteriaLines := []string{}
	collecting := false
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "task objective:") || strings.HasPrefix(lower, "objective:") || strings.HasPrefix(lower, "goal:"):
			collecting = false
			label := "objective:"
			if strings.HasPrefix(lower, "task objective:") {
				label = "task objective:"
			} else if strings.HasPrefix(lower, "goal:") {
				label = "goal:"
			}
			value := strings.TrimSpace(line[len(label):])
			if value == "" && i+1 < len(lines) {
				next := strings.ToLower(strings.TrimSpace(lines[i+1]))
				if !contractCriteria.MatchString(next) {
					value = strings.TrimSpace(lines[i+1])
				}
			}
			if at := strings.Index(strings.ToLower(value), "acceptance criteria:"); at >= 0 {
				part := strings.Trim(value[at+len("acceptance criteria:"):], ". ")
				if part != "" {
					criteriaLines = append(criteriaLines, part)
				}
				value = strings.Trim(value[:at], ". ")
			}
			if objective == "" && value != "" {
				objective = value
			}
		case contractCriteria.MatchString(lower):
			collecting = true
			label := strings.TrimSpace(strings.SplitN(line, ":", 2)[0]) + ":"
			if value := strings.TrimSpace(line[len(label):]); value != "" {
				criteriaLines = append(criteriaLines, value)
			}
		case collecting:
			if strings.HasPrefix(line, "ORCH|") || strings.HasPrefix(line, "TASK_CONTRACT:") || strings.HasPrefix(line, "```") || contractStop.MatchString(lower) {
				collecting = false
			} else if line != "" {
				criteriaLines = append(criteriaLines, contractBullet.ReplaceAllString(line, ""))
			}
		}
	}
	criteria := strings.Join(criteriaLines, "; ")

	values := map[string]string{}
	active := []contractSection{}
	finish := func(section contractSection) {
		parts := []string{}
		for _, raw := range section.lines {
			if text := contractBullet.ReplaceAllString(strings.TrimSpace(raw), ""); text != "" {
				parts = append(parts, text)
			}
		}
		if value := strings.TrimSpace(strings.Join(parts, "; ")); value != "" {
			values[section.kind] = value
		}
	}
	inFence := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		match := contractHead.FindStringSubmatch(line)
		if match != nil {
			level := len(match[1])
			heading := strings.TrimSpace(match[2])
			title, inline, hasColon := strings.Cut(heading, ":")
			title = strings.ToLower(strings.TrimSpace(title))
			kind := ""
			switch title {
			case "objective", "task objective", "task", "goal":
				kind = "objective"
			case "acceptance criteria", "criteria", "requirements":
				kind = "criteria"
			}
			kept := active[:0]
			for _, section := range active {
				if section.level >= level {
					finish(section)
				} else {
					kept = append(kept, section)
				}
			}
			active = kept
			if kind != "" {
				section := contractSection{kind: kind, level: level}
				if hasColon && strings.TrimSpace(inline) != "" {
					section.lines = []string{strings.TrimSpace(inline)}
				}
				active = append(active, section)
			}
			continue
		}
		for i := range active {
			child := false
			for _, other := range active {
				if other.level > active[i].level {
					child = true
					break
				}
			}
			if !child {
				active[i].lines = append(active[i].lines, raw)
			}
		}
	}
	for _, section := range active {
		finish(section)
	}
	if values["objective"] != "" {
		objective = values["objective"]
	}
	if values["criteria"] != "" {
		criteria = values["criteria"]
	}
	if objective != "" && criteria != "" {
		return objective, criteria, true
	}
	if objective == "" {
		objective = defaultObjective
	}
	if criteria == "" {
		criteria = defaultCriteria
	}
	return objective, criteria, false
}

func deriveTaskType(objective, criteria, phase string) string {
	text := strings.ToLower(objective + "\n" + criteria)
	if phase == "operation" && contractCount.MatchString(text) {
		return "counting_status"
	}
	if contractRunbook.MatchString(text) && contractOperation.MatchString(text) {
		return "operation"
	}
	return "unknown"
}
