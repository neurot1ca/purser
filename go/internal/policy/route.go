package policy

import (
	"fmt"
	"sort"
	"strings"
)

type RouteResolution struct {
	Valid   bool    `json:"valid"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Route   any     `json:"route"`
	Model   *string `json:"model"`
	Effort  *string `json:"effort"`
}

type ThreadCompatibilityDecision struct {
	Action     string `json:"action"`
	Compatible bool   `json:"compatible"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

type FreshCodexDecision struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ResolveRoute(route any, config map[string]any, gearUp bool) RouteResolution {
	if err := ValidateConfig(config); err != nil {
		message := err.Error()
		if _, detail, found := strings.Cut(message, ": "); found {
			message = detail
		}
		return RouteResolution{Code: "CONFIG_INVALID", Message: message, Route: route}
	}
	routeName, ok := route.(string)
	routes := config["routes"].(map[string]any)
	entry, exists := routes[routeName]
	if !ok || !exists {
		var returnedRoute any
		if ok {
			returnedRoute = routeName
		}
		return RouteResolution{
			Code:    "INVALID_ENUM_VALUE",
			Message: fmt.Sprintf("Unknown route %s; no model or effort was selected.", pythonRepr(route)),
			Route:   returnedRoute,
		}
	}
	values := entry.(map[string]any)
	model, _ := values["model"].(string)
	effort, _ := values["effort"].(string)
	if gearUp {
		model, effort, _ = GearTarget(config, model, effort)
	}
	return RouteResolution{
		Valid: true, Code: "VALID", Message: "Route is valid.", Route: routeName,
		Model: stringPointer(model), Effort: stringPointer(effort),
	}
}

// GearTarget returns the configured Codex gear-up target for an eligible pair.
func GearTarget(config map[string]any, model, effort string) (string, string, bool) {
	if config["coder_agent"] != nil && config["coder_agent"] != "codex" {
		return model, effort, false
	}
	if !strings.HasSuffix(model, "-luna") || effort != "high" {
		return model, effort, false
	}
	target := configuredSolModel(config)
	if target == "" {
		return model, effort, false
	}
	return target, "medium", true
}

func configuredSolModel(config map[string]any) string {
	routes, _ := config["routes"].(map[string]any)
	counts := make(map[string]int)
	first := make(map[string]int)
	order := gearRouteOrder(config, routes)
	for index, name := range order {
		entry, _ := routes[name].(map[string]any)
		model, _ := entry["model"].(string)
		if strings.HasSuffix(model, "-sol") {
			counts[model]++
			if _, exists := first[model]; !exists {
				first[model] = index
			}
		}
	}
	selected := ""
	selectedCount := 0
	selectedFirst := len(order) + 1
	for model, count := range counts {
		if count > selectedCount ||
			(count == selectedCount && first[model] < selectedFirst) ||
			(count == selectedCount && first[model] == selectedFirst && model < selected) {
			selected, selectedCount, selectedFirst = model, count, first[model]
		}
	}
	return selected
}

func gearRouteOrder(config map[string]any, routes map[string]any) []string {
	order := make([]string, 0, len(routes))
	seen := make(map[string]bool, len(routes))
	if required, ok := config["required_routes"].([]any); ok {
		for _, raw := range required {
			name, ok := raw.(string)
			if ok && !seen[name] {
				if _, exists := routes[name]; exists {
					order = append(order, name)
					seen[name] = true
				}
			}
		}
	}
	remaining := make([]string, 0, len(routes)-len(order))
	for name := range routes {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(order, remaining...)
}

// GearEligibleRoutes returns configured Codex routes whose base pair is eligible.
func GearEligibleRoutes(config map[string]any) []string {
	if config["coder_agent"] != nil && config["coder_agent"] != "codex" {
		return nil
	}
	routes, _ := config["routes"].(map[string]any)
	eligible := make([]string, 0)
	for name, raw := range routes {
		entry, _ := raw.(map[string]any)
		model, _ := entry["model"].(string)
		effort, _ := entry["effort"].(string)
		if strings.HasSuffix(model, "-luna") && effort == "high" {
			eligible = append(eligible, name)
		}
	}
	sort.Strings(eligible)
	return eligible
}

func ThreadCompatibility(requested any, recorded any) ThreadCompatibilityDecision {
	fresh := func(code, message string) ThreadCompatibilityDecision {
		return ThreadCompatibilityDecision{Action: "require a fresh thread", Code: code, Message: message}
	}
	requestedValues, ok := threadValues(requested)
	if !ok {
		return fresh("INVALID_REQUEST", "Requested thread identity is incomplete; a fresh thread is required.")
	}
	if recorded == nil {
		return fresh("NO_LIVE_THREAD", "No compatible live thread was recorded.")
	}
	recordedValues, ok := threadValues(recorded)
	if !ok {
		return fresh("INVALID_LIVE_THREAD", "Recorded live thread identity is incomplete; a fresh thread is required.")
	}
	labels := [...]string{"task id", "route", "model", "effort"}
	for index, label := range labels {
		if requestedValues[index] != recordedValues[index] {
			code := strings.ToUpper(strings.ReplaceAll(label, " ", "_")) + "_MISMATCH"
			return fresh(code, fmt.Sprintf("The recorded live thread has a different %s; use a fresh thread.", label))
		}
	}
	return ThreadCompatibilityDecision{
		Action: "reuse via codex-reply", Compatible: true, Code: "COMPATIBLE",
		Message: "The live thread is compatible; use codex-reply.",
	}
}

func DecideFreshCodex(requested any, recorded any, reason any) FreshCodexDecision {
	compatibility := ThreadCompatibility(requested, recorded)
	if compatibility.Compatible {
		if reason == "user_override" || reason == "reanchor" {
			return FreshCodexDecision{
				Action: "allow fresh codex", Allowed: true, Code: "FRESH_THREAD_OVERRIDE",
				Message: fmt.Sprintf("Explicit reason=%s allows a fresh thread despite a compatible live thread.", reason),
			}
		}
		return FreshCodexDecision{
			Action: "deny fresh codex", Code: "USE_CODEX_REPLY",
			Message: "A compatible live thread exists; use codex-reply instead.",
		}
	}
	message := "No compatible live thread exists; a fresh codex call is allowed."
	if compatibility.Code == "INVALID_REQUEST" {
		message = "Requested thread identity is malformed; allowing a fresh codex call and surfacing the anomaly."
	} else if compatibility.Code == "INVALID_LIVE_THREAD" {
		message = "Recorded live thread identity is corrupt; allowing a fresh codex call and surfacing the anomaly."
	}
	return FreshCodexDecision{Action: "allow fresh codex", Allowed: true, Code: compatibility.Code, Message: message}
}

func threadValues(value any) ([4]string, bool) {
	values := [4]string{}
	identity, ok := value.(map[string]any)
	if !ok {
		return values, false
	}
	for index, key := range [...]string{"task_id", "route", "model", "effort"} {
		field, ok := identity[key].(string)
		if !ok || field == "" {
			return values, false
		}
		values[index] = field
	}
	return values, true
}
