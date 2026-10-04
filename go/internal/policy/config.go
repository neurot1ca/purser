package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	opencodeModel  = regexp.MustCompile(`\A[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\z`)
	agyModel       = regexp.MustCompile(`\Agemini-[A-Za-z0-9._-]+\z`)
	opencodeEffort = regexp.MustCompile(`\A[A-Za-z0-9._-]+\z`)
)

var reviewerSkipReasons = map[string]bool{
	"PHASE_OPERATION": true, "PHASE_PLAN": true, "NO_DELTA": true,
	"BELOW_THRESHOLD": true, "REVIEWER_UNAVAILABLE": true,
}

var reviewerResults = map[string]bool{
	"NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE": true, "CHANGES_REQUIRED": true,
	"UNCERTAIN": true, "REVIEWER1_UNAVAILABLE": true,
}

// ValidateConfig checks the policy configuration using policy.py's config contract.
func ValidateConfig(config map[string]any) error {
	if config == nil {
		return configError("INVALID_CONFIG", "config must be a mapping")
	}
	required := []string{"allowed_phases", "allowed_reasons", "allowed_efforts", "required_routes", "routes", "always_full_routes", "reviewer_gate", "manifest_budget_seconds", "reviewer_skip_reasons", "reviewer_results"}
	missing := make([]string, 0)
	for _, key := range required {
		if _, ok := config[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return configError("MISSING_CONFIG_FIELD", "config is missing required field(s): "+strings.Join(missing, ", "))
	}
	if _, err := stringList(config, "allowed_phases"); err != nil {
		return err
	}
	if _, err := stringList(config, "allowed_reasons"); err != nil {
		return err
	}
	allowedEfforts, err := stringList(config, "allowed_efforts")
	if err != nil {
		return err
	}
	agent, exists := config["coder_agent"]
	if !exists {
		agent = "codex"
	}
	if agent != "codex" && agent != "opencode" && agent != "agy" {
		return configError("INVALID_CONFIG_FIELD", "coder_agent must be codex, opencode or agy")
	}
	allowedEffortSet := stringSet(allowedEfforts)
	requiredRoutes, err := stringList(config, "required_routes")
	if err != nil {
		return err
	}
	requiredRouteSet := stringSet(requiredRoutes)
	routes, ok := config["routes"].(map[string]any)
	if !ok || len(routes) == 0 {
		return configError("INVALID_CONFIG_FIELD", "routes must be a non-empty mapping")
	}
	if !sameKeys(routes, requiredRouteSet) {
		return configError("ROUTE_TABLE_INCOMPLETE", "routes must contain exactly the configured required_routes")
	}
	for route, rawEntry := range routes {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return configError("INVALID_ROUTE_ENTRY", "invalid route entry for "+pythonRepr(route))
		}
		model, modelOK := entry["model"].(string)
		effort := entry["effort"]
		if !modelOK || model == "" {
			return configError("INVALID_ROUTE_ENTRY", "route "+pythonRepr(route)+" has no model")
		}
		if agent == "opencode" && !opencodeModel.MatchString(model) {
			return configError("INVALID_ROUTE_ENTRY", "route "+pythonRepr(route)+" OpenCode model must be provider/model")
		}
		if agent == "agy" && !agyModel.MatchString(model) {
			return configError("INVALID_ROUTE_ENTRY", "route "+pythonRepr(route)+" Antigravity model must be Gemini")
		}
		effortText, effortOK := effort.(string)
		if !effortOK || (agent == "codex" && !allowedEffortSet[effortText]) ||
			(agent == "opencode" && effortText != "none" && !opencodeEffort.MatchString(effortText)) ||
			(agent == "agy" && effortText != "low" && effortText != "medium" && effortText != "high" && effortText != "max") {
			return configError("INVALID_ROUTE_EFFORT", "route "+pythonRepr(route)+" names unsupported effort "+pythonRepr(effort))
		}
	}
	alwaysFull, err := stringList(config, "always_full_routes")
	if err != nil {
		return err
	}
	for _, route := range alwaysFull {
		if _, ok := routes[route]; !ok {
			return configError("INVALID_CONFIG_FIELD", "always_full_routes must name configured routes")
		}
	}
	gate, ok := config["reviewer_gate"].(map[string]any)
	if !ok {
		return configError("INVALID_CONFIG_FIELD", "reviewer_gate must be a mapping")
	}
	for _, key := range []string{"small_delta_max_lines", "small_delta_max_files"} {
		if !nonNegativeInteger(gate[key]) {
			return configError("INVALID_CONFIG_FIELD", key+" must be a non-negative integer")
		}
	}
	if !positiveFiniteNumber(config["manifest_budget_seconds"]) {
		return configError("INVALID_CONFIG_FIELD", "manifest_budget_seconds must be a positive finite number")
	}
	for _, key := range []string{"bundle_non_report_max_bytes", "implementation_report_max_chars", "operation_report_max_chars"} {
		if value, present := config[key]; present && !positiveInteger(value) {
			return configError("INVALID_CONFIG_FIELD", key+" must be a positive integer")
		}
	}
	configuredSkips, err := stringList(config, "reviewer_skip_reasons")
	if err != nil {
		return err
	}
	if !sameStrings(configuredSkips, reviewerSkipReasons) {
		return configError("INVALID_CONFIG_FIELD", "reviewer_skip_reasons must contain the specified skip codes")
	}
	configuredResults, err := stringList(config, "reviewer_results")
	if err != nil {
		return err
	}
	if !sameStrings(configuredResults, reviewerResults) {
		return configError("INVALID_CONFIG_FIELD", "reviewer_results must contain the four specified result codes")
	}
	for _, key := range []string{"micro_repair_timeout_seconds", "max_total_hook_budget_seconds"} {
		if value, present := config[key]; present && !positiveFiniteNumber(value) {
			message := key + " must be a positive finite number"
			return configError("INVALID_CONFIG_FIELD", message)
		}
	}
	if value, present := config["micro_repair_enabled"]; present {
		if _, ok := value.(bool); !ok {
			return configError("INVALID_CONFIG_FIELD", "micro_repair_enabled must be a boolean")
		}
	}
	return nil
}

// LoadConfig validates and returns the same caller-owned mapping, as policy.py does.
func LoadConfig(config map[string]any) (map[string]any, error) {
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	return config, nil
}

func stringList(config map[string]any, key string) ([]string, error) {
	items, ok := config[key].([]any)
	if !ok || len(items) == 0 {
		return nil, configError("INVALID_CONFIG_FIELD", key+" must be a non-empty list of unique non-empty strings")
	}
	result := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok || value == "" || seen[value] {
			return nil, configError("INVALID_CONFIG_FIELD", key+" must be a non-empty list of unique non-empty strings")
		}
		seen[value] = true
		result = append(result, value)
	}
	return result, nil
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sameKeys(values map[string]any, expected map[string]bool) bool {
	if len(values) != len(expected) {
		return false
	}
	for key := range values {
		if !expected[key] {
			return false
		}
	}
	return true
}

func sameStrings(values []string, expected map[string]bool) bool {
	return sameKeys(stringSetAny(values), expected)
}

func stringSetAny(values []string) map[string]any {
	set := make(map[string]any, len(values))
	for _, value := range values {
		set[value] = nil
	}
	return set
}

func positiveInteger(value any) bool {
	switch n := value.(type) {
	case int:
		return n > 0
	case int8:
		return n > 0
	case int16:
		return n > 0
	case int32:
		return n > 0
	case int64:
		return n > 0
	case uint:
		return n > 0
	case uint8:
		return n > 0
	case uint16:
		return n > 0
	case uint32:
		return n > 0
	case uint64:
		return n > 0
	case json.Number:
		parsed, err := strconv.ParseInt(string(n), 10, 64)
		return err == nil && parsed > 0
	default:
		return false
	}
}

func nonNegativeInteger(value any) bool {
	switch n := value.(type) {
	case int:
		return n >= 0
	case int8:
		return n >= 0
	case int16:
		return n >= 0
	case int32:
		return n >= 0
	case int64:
		return n >= 0
	case uint, uint8, uint16, uint32, uint64:
		return true
	case json.Number:
		parsed, err := strconv.ParseInt(string(n), 10, 64)
		if err == nil {
			return parsed >= 0
		}
		_, err = strconv.ParseUint(string(n), 10, 64)
		return err == nil
	default:
		return false
	}
}

func positiveFiniteNumber(value any) bool {
	switch n := value.(type) {
	case int:
		return n > 0
	case int8:
		return n > 0
	case int16:
		return n > 0
	case int32:
		return n > 0
	case int64:
		return n > 0
	case uint:
		return n > 0
	case uint8:
		return n > 0
	case uint16:
		return n > 0
	case uint32:
		return n > 0
	case uint64:
		return n > 0
	case json.Number:
		parsed, err := strconv.ParseFloat(string(n), 64)
		return err == nil && parsed > 0 && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	case float32:
		return n > 0 && !float32Invalid(n)
	case float64:
		return n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
	default:
		return false
	}
}

func float32Invalid(value float32) bool {
	return math.IsNaN(float64(value)) || math.IsInf(float64(value), 0)
}

func configError(code, message string) error { return fmt.Errorf("%s: %s", code, message) }

func pythonRepr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		var b strings.Builder
		quote := byte('\'')
		if strings.ContainsRune(v, '\'') && !strings.ContainsRune(v, '"') {
			quote = '"'
		}
		b.WriteByte(quote)
		for _, r := range v {
			switch r {
			case '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case '\'', '"':
				if byte(r) == quote {
					b.WriteByte('\\')
				}
				b.WriteRune(r)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			case '\b':
				b.WriteString(`\x08`)
			case '\f':
				b.WriteString(`\x0c`)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte(quote)
		return b.String()
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return string(v)
	default:
		return fmt.Sprint(value)
	}
}
