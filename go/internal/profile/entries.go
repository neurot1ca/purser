package profile

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Check is a decoded project check definition. Checks are data only; callers
// decide whether and how to execute them.
type Check struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	When           []string `json:"when"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// AuthoritySource identifies repository documentation and headings to review.
type AuthoritySource struct {
	Path        string   `json:"path"`
	Headings    []string `json:"headings"`
	WhenChanged *string  `json:"when_changed"`
}

var checkNamePattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

var defaultAuthorityHeadings = []string{
	"Authority", "Working scope", "Proportional validation", "Test and comment quality",
}

// DefaultAuthoritySources returns the engine's fallback authority source.
func DefaultAuthoritySources() []AuthoritySource {
	return []AuthoritySource{{Path: "AGENTS.md", Headings: append([]string(nil), defaultAuthorityHeadings...)}}
}

// ParseChecks mirrors project_profile._parse_checks.
func ParseChecks(raw any) ([]Check, string) {
	entries, ok := raw.([]any)
	if !ok || len(entries) > 8 {
		return []Check{}, "[[checks]] must be an array of at most 8 tables"
	}
	checks := make([]Check, 0, len(entries))
	names := make(map[string]bool)
	for index, item := range entries {
		prefix := fmt.Sprintf("[[checks]] entry %d", index)
		entry, ok := item.(map[string]any)
		if !ok || hasUnknownKeys(entry, "name", "argv", "when", "timeout_seconds") {
			return []Check{}, prefix + " has invalid keys or type"
		}
		name, ok := entry["name"].(string)
		if !ok || !checkNamePattern.MatchString(name) || names[name] {
			return []Check{}, prefix + " name is invalid or duplicate"
		}
		argv, ok := stringArray(entry["argv"], true)
		if !ok {
			return []Check{}, prefix + " argv must be a non-empty array of non-empty strings"
		}
		var when []string
		if value, present := entry["when"]; present && value != nil {
			when, ok = stringArray(value, false)
			if !ok {
				return []Check{}, prefix + " when must contain repository-relative globs"
			}
			for _, pattern := range when {
				if _, valid := AuthorityPath(pattern); !valid {
					return []Check{}, prefix + " when must contain repository-relative globs"
				}
			}
		}
		timeout := 60
		if value, present := entry["timeout_seconds"]; present {
			integer, valid := integerOnly(value)
			if !valid || integer < 1 || integer > 300 {
				return []Check{}, prefix + " timeout_seconds must be an integer from 1 to 300"
			}
			timeout = integer
		}
		names[name] = true
		checks = append(checks, Check{Name: name, Argv: argv, When: when, TimeoutSeconds: timeout})
	}
	return checks, ""
}

// ParseAuthoritySources mirrors project_profile._parse_authority_sources.
func ParseAuthoritySources(tier string, raw any) ([]AuthoritySource, []string) {
	entries, ok := raw.([]any)
	if !ok {
		return []AuthoritySource{}, []string{tier + ": [authority] sources must be a list; ignored"}
	}
	sources := make([]AuthoritySource, 0, len(entries))
	warnings := make([]string, 0)
	for index, item := range entries {
		prefix := fmt.Sprintf("%s: [authority] sources[%d]", tier, index)
		entry, ok := item.(map[string]any)
		if !ok {
			warnings = append(warnings, prefix+" must be a table; entry ignored")
			continue
		}
		unknown := unknownKeys(entry, "path", "headings", "when_changed")
		if len(unknown) > 0 {
			quoted := make([]string, len(unknown))
			for i, key := range unknown {
				quoted[i] = "'" + strings.ReplaceAll(key, "'", "\\'") + "'"
			}
			warnings = append(warnings, fmt.Sprintf("%s has unknown key(s) [%s]; entry ignored", prefix, strings.Join(quoted, ", ")))
			continue
		}
		path, valid := AuthorityPath(entry["path"])
		if !valid {
			warnings = append(warnings, prefix+".path must be a relative repository path without '..'; entry ignored")
			continue
		}
		headings, valid := stringArray(entry["headings"], true)
		if !valid {
			warnings = append(warnings, prefix+".headings must be a non-empty list of strings; entry ignored")
			continue
		}
		var whenChanged *string
		if value, present := entry["when_changed"]; present {
			text, ok := value.(string)
			if !ok {
				warnings = append(warnings, prefix+".when_changed must be a string; entry ignored")
				continue
			}
			whenChanged = &text
		}
		sources = append(sources, AuthoritySource{Path: path, Headings: headings, WhenChanged: whenChanged})
	}
	return sources, warnings
}

// AuthorityPath normalizes a safe repository-relative path.
func AuthorityPath(value any) (string, bool) {
	path, ok := value.(string)
	if !ok || path == "" {
		return "", false
	}
	normalized := strings.ReplaceAll(path, `\`, "/")
	for strings.HasPrefix(normalized, "./") {
		normalized = strings.TrimPrefix(normalized, "./")
	}
	if normalized == "" || strings.HasPrefix(normalized, "/") || strings.Contains(strings.ToLower(path[:min(len(path), 2)]), ":") {
		return "", false
	}
	for _, component := range strings.Split(normalized, "/") {
		if component == ".." {
			return "", false
		}
	}
	return normalized, true
}

func hasUnknownKeys(entry map[string]any, allowed ...string) bool {
	return len(unknownKeys(entry, allowed...)) > 0
}

func unknownKeys(entry map[string]any, allowed ...string) []string {
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	unknown := make([]string, 0)
	for key := range entry {
		if !set[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func stringArray(value any, nonEmpty bool) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || nonEmpty && len(items) == 0 {
		return nil, false
	}
	result := make([]string, len(items))
	for i, item := range items {
		text, ok := item.(string)
		if !ok || nonEmpty && text == "" {
			return nil, false
		}
		result[i] = text
	}
	return result, true
}

func integerOnly(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), int64(int(number)) == number
	case int32:
		return int(number), true
	case int16:
		return int(number), true
	case int8:
		return int(number), true
	case json.Number:
		integer, err := number.Int64()
		return int(integer), err == nil && int64(int(integer)) == integer
	}
	return 0, false
}
