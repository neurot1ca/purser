package profile

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var familyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var userSecurity = map[string]map[string]bool{
	"roles":    {"orchestrator": true, "coder": true, "reviewer": true},
	"coding":   {"coder_agent": true, "routes": true, "family": true, "reviewer1_agent": true, "reviewer1_model_requested": true, "reviewer1_effort": true, "reviewer1_executable": true},
	"reviewer": {"executable": true, "selected": true, "alternatives": true},
	"trust":    {"paths": true},
	"paths":    {"protected": true},
	"review":   {"force_full": true, "skip_max_lines": true, "skip_max_files": true},
}

var projectSecurity = map[string]map[string]bool{
	"paths":  {"protected": true},
	"review": {"force_full": true, "skip_max_lines": true, "skip_max_files": true, "full_suite_required": true},
}

// ValidateSecurityKey reports whether a key is a security value for the tier.
func ValidateSecurityKey(tier, section, key string) (bool, string) {
	sections := userSecurity
	if tier == "project" {
		sections = projectSecurity
	} else if tier != "user" {
		return false, unknownSecurityKey(section, key)
	}
	if keys, ok := sections[section]; ok && keys[key] {
		return true, ""
	}
	return false, unknownSecurityKey(section, key)
}

func unknownSecurityKey(section, key string) string {
	return fmt.Sprintf("[%s] unknown security key '%s'", section, strings.ReplaceAll(key, "'", "\\'"))
}

// ValidateSecurityValue mirrors project_profile._valid_security_value.
func ValidateSecurityValue(section, key string, value any) (bool, string) {
	invalid := func() (bool, string) {
		return false, fmt.Sprintf("[%s] %s has an invalid type or value", section, key)
	}
	switch section {
	case "coding":
		valid := false
		switch key {
		case "coder_agent":
			valid = oneOf(value, "codex", "opencode", "agy", "claude", "cursor")
		case "reviewer1_agent":
			valid = oneOf(value, "codex", "opencode", "agy", "claude", "cursor")
		case "reviewer1_model_requested":
			if value == nil {
				valid = true
			} else if model, ok := value.(string); ok {
				valid = model != ""
			}
		case "reviewer1_effort":
			valid = oneOf(value, "low", "medium", "high", "xhigh", "max", "none")
		case "reviewer1_executable":
			if executable, ok := value.(string); ok {
				valid = isHostAbsolute(executable)
			}
		case "family":
			if v, ok := value.(string); ok {
				valid = familyPattern.MatchString(v)
			}
		default:
			if routes, ok := value.(map[string]any); ok {
				valid = true
				for _, route := range routes {
					if _, ok := route.(string); ok {
						continue
					}
					model, ok := route.(map[string]any)
					if !ok || len(model) != 2 {
						valid = false
						break
					}
					_, modelNameOK := model["model"].(string)
					_, effortOK := model["effort"].(string)
					if !modelNameOK || !effortOK {
						valid = false
						break
					}
				}
			}
		}
		if valid {
			return true, ""
		}
		return false, fmt.Sprintf("[coding] %s has an invalid value", key)
	case "roles":
		allowed := map[string][]string{
			"orchestrator": {"claude-code"},
			"coder":        {"codex", "claude", "opencode", "agy", "cursor"},
			"reviewer":     {"agy", "opencode", "codex", "claude", "cursor", "none"},
		}
		if choices, ok := allowed[key]; ok && valueIsOneOf(value, choices) {
			return true, ""
		}
		return false, fmt.Sprintf("[roles] %s has an invalid type or value", key)
	case "review":
		if key == "force_full" || key == "full_suite_required" {
			if _, ok := value.(bool); ok {
				return true, ""
			}
		} else if integer, ok := integerValue(value); ok && integer >= 0 {
			return true, ""
		}
		return false, fmt.Sprintf("[review] %s has an invalid type or value", key)
	case "reviewer":
		if key == "executable" {
			if path, ok := value.(string); ok && isHostAbsolute(path) {
				return true, ""
			}
			return false, "[reviewer] executable must be an absolute path string"
		}
		if key == "alternatives" {
			candidates, ok := value.([]any)
			if !ok {
				return false, "[reviewer] alternatives must be an array of candidate tables"
			}
			for _, candidate := range candidates {
				if !validCandidate(candidate) {
					return false, "[reviewer] alternatives entries must contain non-empty agent and model strings"
				}
			}
			return true, ""
		}
		candidate, ok := value.(map[string]any)
		if !ok {
			return false, "[reviewer] selected must be an agent/model table"
		}
		if !validCandidate(candidate) {
			return false, "[reviewer] selected entries must contain non-empty agent and model strings"
		}
		return true, ""
	case "trust":
		if key == "paths" {
			paths, ok := value.([]any)
			if ok {
				for _, item := range paths {
					path, ok := item.(string)
					if !ok || !isHostAbsolute(path) {
						return false, "[trust] paths must be an array of absolute path strings"
					}
				}
				return true, ""
			}
			return false, "[trust] paths must be an array of absolute path strings"
		}
	case "paths":
		if key == "protected" {
			paths, ok := value.([]any)
			if ok {
				for _, item := range paths {
					if _, ok := item.(string); !ok {
						return false, "[paths] protected must be an array of strings"
					}
				}
				return true, ""
			}
			return false, "[paths] protected must be an array of strings"
		}
	}
	return invalid()
}

func oneOf(value any, choices ...string) bool {
	text, ok := value.(string)
	return ok && valueIsOneOf(text, choices)
}

func valueIsOneOf(value any, choices []string) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	for _, choice := range choices {
		if text == choice {
			return true
		}
	}
	return false
}

func validCandidate(value any) bool {
	candidate, ok := value.(map[string]any)
	if !ok || (len(candidate) != 2 && len(candidate) != 3) {
		return false
	}
	for key := range candidate {
		if key != "agent" && key != "model" && key != "effort" {
			return false
		}
	}
	agent, agentOK := candidate["agent"].(string)
	model, modelOK := candidate["model"].(string)
	if !agentOK || !modelOK || len(agent) == 0 || len(agent) > 64 || len(model) == 0 || len(model) > 128 {
		return false
	}
	if effort, exists := candidate["effort"]; exists {
		value, ok := effort.(string)
		return ok && (value == "low" || value == "medium" || value == "high" || value == "xhigh" || value == "max")
	}
	return agent != "claude"
}

func integerValue(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case int16:
		return int64(v), true
	case int8:
		return int64(v), true
	case json.Number:
		text := string(v)
		if text == "-0" {
			return 0, true
		}
		if text == "" || strings.HasPrefix(text, "-") {
			return 0, false
		}
		for _, char := range text {
			if char < '0' || char > '9' {
				return 0, false
			}
		}
		return 0, true
	}
	return 0, false
}

func isHostAbsolute(path string) bool {
	return filepath.IsAbs(path) || runtime.GOOS == "windows" && (strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`))
}
