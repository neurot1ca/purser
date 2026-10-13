package profile

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Tier is the result of validating an already-decoded profile table.
type Tier struct {
	Status   string         `json:"status"`
	Values   map[string]any `json:"values"`
	Warnings []string       `json:"warnings"`
}

var userNonSecurity = map[string]map[string]bool{"telemetry": {"vault_root": true}}
var projectNonSecurity = map[string]map[string]bool{
	"project": {"id": true, "measurement_ledger": true}, "authority": {"sources": true},
	"worktree":    {"copy": true, "setup": true, "setup_timeout_seconds": true},
	"maintenance": {"coverage_target_pct": true},
	"validation":  {"command": true},
}

// ValidateTier mirrors project_profile._load_tier after TOML decoding. Pass
// decoder key paths to retain source order; map-only callers retain sorted order.
func ValidateTier(tier string, data map[string]any, sourceKeys ...[]toml.Key) Tier {
	if len(sourceKeys) == 0 {
		return validateTier(tier, data, nil)
	}
	rootOrder, sectionOrder := keyOrders(sourceKeys[0])
	return validateTier(tier, data, &tierKeyOrder{root: rootOrder, sections: sectionOrder})
}

type tierKeyOrder struct {
	root     []string
	sections map[string][]string
}

func keyOrders(keys []toml.Key) ([]string, map[string][]string) {
	root := []string{}
	sections := map[string][]string{}
	seenRoot := map[string]bool{}
	seenSections := map[string]map[string]bool{}
	for _, key := range keys {
		if len(key) == 0 {
			continue
		}
		if !seenRoot[key[0]] {
			seenRoot[key[0]] = true
			root = append(root, key[0])
		}
		if len(key) > 1 {
			if seenSections[key[0]] == nil {
				seenSections[key[0]] = map[string]bool{}
			}
			if !seenSections[key[0]][key[1]] {
				seenSections[key[0]][key[1]] = true
				sections[key[0]] = append(sections[key[0]], key[1])
			}
		}
	}
	return root, sections
}

func validateTier(tier string, data map[string]any, order *tierKeyOrder) Tier {
	reject := func(reason string, warnings []string) Tier {
		warnings = append(warnings, tier+" tier rejected: "+reason)
		return Tier{Status: "rejected", Values: map[string]any{}, Warnings: warnings}
	}
	if tier != "user" && tier != "project" {
		return reject("unknown profile tier", nil)
	}
	warnings := []string{}
	schema := data["schema_version"]
	if !isSchemaVersionOne(schema) {
		return reject("schema_version must be 1", warnings)
	}
	security := userSecurity
	nonSecurity := userNonSecurity
	securityOrder := []string{"roles", "coding", "reviewer", "trust", "paths", "review"}
	nonSecurityOrder := []string{"telemetry"}
	if tier == "project" {
		security = projectSecurity
		nonSecurity = projectNonSecurity
		securityOrder = []string{"paths", "review"}
		nonSecurityOrder = []string{"project", "authority", "worktree", "maintenance", "validation"}
	}
	if tier == "user" {
		if _, exists := data["checks"]; exists {
			return reject("[[checks]] is project-only", warnings)
		}
	}
	known := map[string]bool{"schema_version": true}
	for section := range security {
		known[section] = true
	}
	for section := range nonSecurity {
		known[section] = true
	}
	if tier == "project" {
		known["checks"] = true
	} else {
		known["mechanical"] = true
	}
	for _, key := range orderedKeys(data, order, "") {
		if !known[key] {
			warnings = append(warnings, fmt.Sprintf("%s: unknown top-level key or section [%s] ignored", tier, key))
		}
	}
	values := map[string]any{}
	if tier == "project" {
		if raw, exists := data["checks"]; exists {
			checks, reason := ParseChecks(raw)
			if reason != "" {
				return reject(reason, warnings)
			}
			values["checks"] = checks
		}
	}
	for _, sectionName := range securityOrder {
		section, exists := data[sectionName]
		if !exists {
			continue
		}
		table, ok := section.(map[string]any)
		if !ok {
			return reject(fmt.Sprintf("[%s] security section must be a table", sectionName), warnings)
		}
		for _, key := range orderedKeys(table, order, sectionName) {
			value := table[key]
			if !security[sectionName][key] {
				return reject(fmt.Sprintf("[%s] unknown security key '%s'", sectionName, strings.ReplaceAll(key, "'", "\\'")), warnings)
			}
			valid, reason := ValidateSecurityValue(sectionName, key, value)
			if !valid {
				return reject(reason, warnings)
			}
			values[sectionName+"."+key] = value
		}
	}
	for _, sectionName := range nonSecurityOrder {
		section, exists := data[sectionName]
		if !exists {
			continue
		}
		table, ok := section.(map[string]any)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: [%s] must be a table; ignored", tier, sectionName))
			continue
		}
		for _, key := range orderedKeys(table, order, sectionName) {
			value := table[key]
			if !nonSecurity[sectionName][key] {
				warnings = append(warnings, fmt.Sprintf("%s: [%s] unknown key '%s' ignored", tier, sectionName, strings.ReplaceAll(key, "'", "\\'")))
				continue
			}
			if sectionName == "authority" && key == "sources" {
				sources, sourceWarnings := ParseAuthoritySources(tier, value)
				warnings = append(warnings, sourceWarnings...)
				values["authority.sources"] = sources
				continue
			}
			if sectionName == "worktree" {
				switch key {
				case "copy":
					if paths, valid := nonEmptyStringArray(value); valid {
						values["worktree.copy"] = paths
					} else {
						warnings = append(warnings, tier+": [worktree] copy must be a list of non-empty strings; ignored")
					}
				case "setup":
					if setup, valid := value.(string); valid && strings.TrimSpace(setup) != "" {
						values["worktree.setup"] = setup
					} else {
						warnings = append(warnings, tier+": [worktree] setup must be a non-empty string; ignored")
					}
				default:
					if seconds, valid := tierInteger(value); valid && seconds >= 1 && seconds <= 3600 {
						values["worktree.setup_timeout_seconds"] = seconds
					} else {
						warnings = append(warnings, tier+": [worktree] setup_timeout_seconds must be an integer from 1 to 3600; ignored")
					}
				}
				continue
			}
			if sectionName == "maintenance" && key == "coverage_target_pct" {
				if target, valid := profileNumber(value); valid && target > 0 && target <= 100 {
					values["maintenance.coverage_target_pct"] = target
				} else {
					warnings = append(warnings, tier+": [maintenance] coverage_target_pct must be a finite number greater than 0 and at most 100; using 90")
				}
				continue
			}
			if sectionName == "validation" {
				if command, valid := value.(string); valid && strings.TrimSpace(command) != "" {
					values["validation.command"] = command
				} else {
					warnings = append(warnings, tier+": [validation] command must be a non-empty string; ignored")
				}
				continue
			}
			if text, valid := value.(string); valid {
				values[sectionName+"."+key] = text
			} else {
				warnings = append(warnings, fmt.Sprintf("%s: [%s] %s must be a string; ignored", tier, sectionName, key))
			}
		}
	}
	return Tier{Status: "ok", Values: values, Warnings: warnings}
}

func profileNumber(value any) (float64, bool) {
	var number float64
	switch value := value.(type) {
	case int64:
		number = float64(value)
	case float64:
		number = value
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func orderedKeys(values map[string]any, order *tierKeyOrder, section string) []string {
	if order == nil {
		return sortedKeys(values)
	}
	keys := make([]string, 0, len(values))
	seen := map[string]bool{}
	ordered := order.root
	if section != "" {
		ordered = order.sections[section]
	}
	for _, key := range ordered {
		if _, exists := values[key]; exists && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	if len(seen) < len(values) {
		return sortedKeys(values)
	}
	return keys
}

func isSchemaVersionOne(value any) bool {
	if integer, ok := tierInteger(value); ok {
		return integer == 1
	}
	return false
}

func tierInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int64:
		return number, true
	case json.Number:
		integer, err := strconv.ParseInt(string(number), 10, 64)
		return integer, err == nil
	}
	return 0, false
}

func nonEmptyStringArray(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]string, len(items))
	for i, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, false
		}
		result[i] = text
	}
	return result, true
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
