package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"path/filepath"

	"purser-hook/internal/profile"
)

// LoadCoderConfig loads shipped defaults and the initial Codex user overlay.
func LoadCoderConfig(configPath, userPath string) (map[string]any, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	config := map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	userRaw, err := os.ReadFile(userPath)
	if os.IsNotExist(err) {
		return config, validateHookConfig(config)
	}
	if err != nil {
		return nil, err
	}
	user, _, err := profile.DecodeTOML(userRaw)
	if err != nil {
		return nil, err
	}
	mechanical, ok := user["mechanical"]
	if ok {
		table, valid := mechanical.(map[string]any)
		if !valid {
			return nil, fmt.Errorf("[mechanical] opencode_enabled must be a boolean")
		}
		enabled, exists := table["opencode_enabled"]
		if exists {
			if _, valid := enabled.(bool); !valid {
				return nil, fmt.Errorf("[mechanical] opencode_enabled must be a boolean")
			}
		} else {
			enabled = true
		}
		config["mechanical_opencode_enabled"] = enabled
	} else {
		config["mechanical_opencode_enabled"] = true
	}
	coding, exists := user["coding"]
	table := map[string]any{}
	if exists {
		var valid bool
		table, valid = coding.(map[string]any)
		if !valid {
			return nil, fmt.Errorf("[coding] must be a table")
		}
		if model, exists := table["reviewer1_model_requested"]; exists && model != nil {
			text, valid := model.(string)
			agent, _ := table["reviewer1_agent"].(string)
			if agent == "" {
				agent = "agy"
			}
			if !valid || (agent == "claude" && !claudeModel.MatchString(text)) || (agent != "claude" && !agyModel.MatchString(text)) {
				return nil, fmt.Errorf("[coding] reviewer1_model_requested is invalid")
			}
			config["reviewer1_model_requested"] = text
		}
		if raw, exists := table["reviewer1_agent"]; exists {
			agent, valid := raw.(string)
			if !valid || (agent != "agy" && agent != "claude" && agent != "codex" && agent != "opencode") {
				return nil, fmt.Errorf("[coding] reviewer1_agent is invalid")
			}
			config["reviewer1_agent"] = agent
		}
		if raw, exists := table["reviewer1_effort"]; exists {
			effort, valid := raw.(string)
			if !valid || !claudeEfforts[effort] {
				return nil, fmt.Errorf("[coding] reviewer1_effort is invalid")
			}
			config["reviewer1_effort"] = effort
		}
		if table["reviewer1_agent"] == "claude" {
			if _, ok := table["reviewer1_model_requested"].(string); !ok || config["reviewer1_effort"] == nil {
				return nil, fmt.Errorf("[coding] Claude reviewer requires model and effort")
			}
		}
	}
	if err := loadReadiness(config, table); err != nil {
		return nil, err
	}
	agent := "codex"
	if value, exists := table["coder_agent"]; exists {
		var valid bool
		agent, valid = value.(string)
		if !valid || (agent != "codex" && agent != "opencode" && agent != "agy") {
			return nil, fmt.Errorf("[coding] coder_agent is invalid")
		}
	}
	if executable, exists := table["reviewer1_executable"]; exists && executable != nil {
		text, valid := executable.(string)
		if !valid || !filepath.IsAbs(text) {
			return nil, fmt.Errorf("[coding] reviewer1_executable must be an absolute path")
		}
		config["reviewer1_executable"] = text
	}
	role := "codex"
	if roles, exists := user["roles"]; exists {
		roleTable, valid := roles.(map[string]any)
		if !valid {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pythonTypeName(roles))
		}
		if value, exists := roleTable["coder"]; exists {
			role, valid = value.(string)
			if !valid {
				return nil, fmt.Errorf("[coding] coder routes or role are invalid")
			}
		}
	}
	if family, exists := table["family"]; exists {
		text, valid := family.(string)
		if (agent != "opencode" && agent != "agy") || !valid || !coderFamily.MatchString(text) {
			return nil, fmt.Errorf("[coding] family is invalid")
		}
		config["coder_family"] = text
	}
	routes, routesExist := table["routes"]
	if agent == "opencode" || agent == "agy" {
		routeMap, valid := routes.(map[string]any)
		required, _ := config["required_routes"].([]any)
		if role != agent || !routesExist || !valid || len(routeMap) != len(required) || !hasRouteKeys(routeMap, required) {
			return nil, fmt.Errorf("[coding] coder routes or role are invalid")
		}
		models := map[string]bool{}
		for _, route := range required {
			name := route.(string)
			if agent == "opencode" {
				model, valid := routeMap[name].(string)
				if !valid || !opencodeModel.MatchString(model) {
					return nil, fmt.Errorf("[coding] coder route model is invalid")
				}
				models[model] = true
				continue
			}
			entry, valid := routeMap[name].(map[string]any)
			model, modelOK := entry["model"].(string)
			_effort, effortOK := entry["effort"].(string)
			if !valid || len(entry) != 2 || !modelOK || !agyModel.MatchString(model) || !effortOK || !containsString([]any{"low", "medium", "high", "max"}, _effort) {
				return nil, fmt.Errorf("[coding] Antigravity route model or effort is invalid")
			}
			models[model] = true
		}
		if len(models) != 1 {
			return nil, fmt.Errorf("[coding] coder routes must use one model")
		}
		if agent == "opencode" {
			converted := map[string]any{}
			for name, raw := range routeMap {
				converted[name] = map[string]any{"model": raw, "effort": "none"}
			}
			routeMap = converted
		}
		config["routes"] = routeMap
	} else {
		if role == "opencode" || role == "agy" {
			return nil, fmt.Errorf("[coding] coder agent and routes disagree")
		}
		routeMap, valid := routes.(map[string]any)
		required, _ := config["required_routes"].([]any)
		if routesExist && (!valid || (len(routeMap) > 0 && (role != "codex" || len(routeMap) != len(required) || !hasRouteKeys(routeMap, required)))) {
			return nil, fmt.Errorf("[coding] coder routes or role are invalid")
		}
		if len(routeMap) > 0 {
			for _, route := range required {
				name := route.(string)
				entry, valid := routeMap[name].(map[string]any)
				model, modelOK := entry["model"].(string)
				effort, effortOK := entry["effort"].(string)
				allowed, _ := config["allowed_efforts"].([]any)
				if !valid || len(entry) != 2 || !modelOK || !codexModel.MatchString(model) || !effortOK || !containsString(allowed, effort) {
					return nil, fmt.Errorf("[coding] Codex route model or effort is invalid")
				}
			}
			config["routes"] = routeMap
		}
	}
	config["coder_agent"] = agent
	if fallback, exists := user["fallback"]; exists {
		table, valid := fallback.(map[string]any)
		_, hasEffort := table["effort"]
		if !valid || (len(table) != 3 && (len(table) != 4 || !hasEffort)) || !hasStringKeys(table, []string{"agent", "model", "family"}) {
			return nil, fmt.Errorf("[fallback] agent, model, or family is invalid")
		}
		fallbackAgent, agentOK := table["agent"].(string)
		model, modelOK := table["model"].(string)
		family, familyOK := table["family"].(string)
		effort := ""
		effortOK := true
		if raw, exists := table["effort"]; exists {
			effort, effortOK = raw.(string)
		}
		if !agentOK || (fallbackAgent != "codex" && fallbackAgent != "opencode" && fallbackAgent != "agy" && fallbackAgent != "claude") || !modelOK || !familyOK || !effortOK || !coderFamily.MatchString(family) {
			return nil, fmt.Errorf("[fallback] agent, model, or family is invalid")
		}
		if effort != "" && (fallbackAgent != "claude" || !claudeEfforts[effort]) {
			return nil, fmt.Errorf("[fallback] effort is unsupported for this agent")
		}
		if fallbackAgent == "claude" && family != "anthropic" && family != "claude" {
			return nil, fmt.Errorf("[fallback] Claude family must be anthropic")
		}
		pattern := codexModel
		if fallbackAgent == "opencode" {
			pattern = opencodeModel
		} else if fallbackAgent == "agy" {
			pattern = agyModel
		} else if fallbackAgent == "claude" {
			pattern = claudeModel
			if !claudeEfforts[effort] {
				return nil, fmt.Errorf("[fallback] Claude effort is invalid")
			}
		}
		if !pattern.MatchString(model) {
			return nil, fmt.Errorf("[fallback] model is invalid")
		}
		routes, _ := config["routes"].(map[string]any)
		primary := routes["normal"]
		primaryModel := ""
		if entry, ok := primary.(map[string]any); ok {
			primaryModel, _ = entry["model"].(string)
		} else if text, ok := primary.(string); ok {
			primaryModel = text
		}
		if model == primaryModel {
			return nil, fmt.Errorf("[fallback] model must differ from the primary coder model")
		}
		config["fallback"] = map[string]any{"agent": fallbackAgent, "model": model, "family": family}
		if effort != "" {
			config["fallback"].(map[string]any)["effort"] = effort
		}
	}
	return config, validateHookConfig(config)
}

func validateHookConfig(config map[string]any) error {
	if err := ValidateConfig(config); err != nil {
		return err
	}
	if field, _ := config["post_tool_output_field"].(string); field != "updatedToolOutput" && field != "updatedMCPToolOutput" {
		return fmt.Errorf("post_tool_output_field is not a supported hook field")
	}
	model, ok := config["reviewer1_model_requested"].(string)
	if !ok || model == "" {
		return fmt.Errorf("reviewer1_model_requested must be a non-empty string")
	}
	deadline, exists := config["hook_checks_deadline_seconds"]
	if !exists {
		deadline = 150
	}
	if !positiveFiniteNumber(deadline) {
		return fmt.Errorf("hook_checks_deadline_seconds must be positive")
	}
	return nil
}

var codexModel = regexp.MustCompile(`\A[A-Za-z0-9._-]+\z`)
var claudeModel = regexp.MustCompile(`\Aclaude-[A-Za-z0-9._-]+\z`)
var claudeEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
var coderFamily = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._-]{0,63}\z`)

func loadReadiness(config, coding map[string]any) error {
	readiness, ok := coding["readiness_check"]
	if !ok {
		readiness = map[string]any{}
	}
	table, valid := readiness.(map[string]any)
	if !valid {
		return fmt.Errorf("[coding.readiness_check] accepts only agent and model")
	}
	for key := range table {
		if key != "agent" && key != "model" {
			return fmt.Errorf("[coding.readiness_check] accepts only agent and model")
		}
	}
	agent := "agy"
	model, _ := config["readiness_check"].(map[string]any)
	modelValue := model["model"]
	if value, ok := table["agent"]; ok {
		var valid bool
		agent, valid = value.(string)
		if !valid {
			return fmt.Errorf("[coding.readiness_check] agent must be agy or off")
		}
	}
	if value, ok := table["model"]; ok {
		modelValue = value
	}
	if agent != "agy" && agent != "off" {
		return fmt.Errorf("[coding.readiness_check] agent must be agy or off")
	}
	modelText, ok := modelValue.(string)
	if !ok || !agyModel.MatchString(modelText) {
		return fmt.Errorf("[coding.readiness_check] model is invalid")
	}
	config["readiness_check"] = map[string]any{"agent": agent, "model": modelText}
	return nil
}

func hasRouteKeys(routes map[string]any, required []any) bool {
	for _, key := range required {
		name, ok := key.(string)
		if !ok {
			return false
		}
		if _, ok := routes[name]; !ok {
			return false
		}
	}
	return true
}
func containsString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

var (
	opencodeModel  = regexp.MustCompile(`\A[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\z`)
	agyModel       = regexp.MustCompile(`\Agemini-[A-Za-z0-9._-]+\z`)
	opencodeEffort = regexp.MustCompile(`\A[A-Za-z0-9._-]+\z`)
)

func hasStringKeys(values map[string]any, keys []string) bool {
	for _, key := range keys {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}

func pythonTypeName(value any) string {
	switch value.(type) {
	case string:
		return "str"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return "int"
	case float32, float64:
		return "float"
	case []any:
		return "list"
	default:
		return "NoneType"
	}
}

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
