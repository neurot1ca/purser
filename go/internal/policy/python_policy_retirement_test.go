package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func policyTestConfig(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	return config
}

func clonePolicyConfig(t *testing.T, config map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	clone := map[string]any{}
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func TestPythonRouteAndConfigAssertions(t *testing.T) {
	config := policyTestConfig(t)
	config["coder_agent"] = "opencode"
	delete(config, "sol_model")
	for _, raw := range config["routes"].(map[string]any) {
		raw.(map[string]any)["model"] = "opencode/big-pickle"
		raw.(map[string]any)["effort"] = "none"
	}
	if err := ValidateConfig(config); err != nil {
		t.Fatalf("qualified OpenCode routes rejected: %v", err)
	}
	if got := ResolveRoute("normal", config, false); !got.Valid || *got.Model != "opencode/big-pickle" || *got.Effort != "none" {
		t.Fatalf("OpenCode route = %#v", got)
	}
	config["routes"].(map[string]any)["normal"].(map[string]any)["model"] = "big-pickle"
	if got := ResolveRoute("normal", config, false); got.Code != "CONFIG_INVALID" {
		t.Fatalf("unqualified OpenCode model result = %#v", got)
	}
	config["routes"].(map[string]any)["normal"].(map[string]any)["model"] = "opencode/big-pickle"
	config["routes"].(map[string]any)["normal"].(map[string]any)["effort"] = "bad variant"
	if err := ValidateConfig(config); err == nil {
		t.Fatal("invalid OpenCode effort was accepted")
	}

	config = policyTestConfig(t)
	for _, raw := range config["routes"].(map[string]any) {
		raw.(map[string]any)["model"] = "gpt-6-astra"
		raw.(map[string]any)["effort"] = "ultra"
	}
	if err := ValidateConfig(config); err != nil {
		t.Fatalf("complete non-Sol routes rejected: %v", err)
	}
	delete(config["routes"].(map[string]any), "normal")
	if err := ValidateConfig(config); err == nil || !strings.Contains(err.Error(), "ROUTE_TABLE_INCOMPLETE") {
		t.Fatalf("incomplete route table error = %v", err)
	}
	if got := ResolveRoute("normal", config, false); got.Code != "CONFIG_INVALID" {
		t.Fatalf("invalid config route result = %#v", got)
	}

	config = policyTestConfig(t)
	config["coder_agent"] = "agy"
	for _, raw := range config["routes"].(map[string]any) {
		raw.(map[string]any)["model"] = "gemini-3.8-flash-low"
		raw.(map[string]any)["effort"] = "medium"
	}
	if got := ResolveRoute("normal", config, false); !got.Valid || *got.Model != "gemini-3.8-flash-low" || *got.Effort != "medium" {
		t.Fatalf("valid AGY route = %#v", got)
	}
	config["routes"].(map[string]any)["normal"].(map[string]any)["effort"] = "none"
	if got := ResolveRoute("normal", config, false); got.Code != "CONFIG_INVALID" {
		t.Fatalf("invalid AGY effort result = %#v", got)
	}
	config["routes"].(map[string]any)["normal"].(map[string]any)["effort"] = "max"
	if got := ResolveRoute("normal", config, false); !got.Valid || *got.Effort != "max" {
		t.Fatalf("maximum AGY effort route = %#v", got)
	}
	config["routes"].(map[string]any)["normal"].(map[string]any)["model"] = "claude-sonnet-4-6"
	if got := ResolveRoute("normal", config, false); got.Code != "CONFIG_INVALID" {
		t.Fatalf("non-Gemini AGY model result = %#v", got)
	}

	config = policyTestConfig(t)
	want := map[string][2]string{
		"mechanical": {"gpt-6-luna", "high"}, "subtle": {"gpt-6-luna", "high"},
		"normal": {"gpt-6.1-sol", "medium"}, "consequential": {"gpt-6.1-sol", "high"},
		"escalated": {"gpt-6.1-sol", "xhigh"}, "operation": {"gpt-6-luna", "high"},
	}
	for route, pair := range want {
		got := ResolveRoute(route, config, false)
		if !got.Valid || *got.Model != pair[0] || *got.Effort != pair[1] {
			t.Errorf("ResolveRoute(%q) = %#v, want %s/%s", route, got, pair[0], pair[1])
		}
	}
	if got := ResolveRoute("future-route", config, false); got.Valid || got.Model != nil || got.Effort != nil {
		t.Fatalf("unknown route selected a fallback: %#v", got)
	}

	for _, field := range []string{"manifest_budget_seconds", "bundle_non_report_max_bytes", "implementation_report_max_chars", "operation_report_max_chars"} {
		invalid := clonePolicyConfig(t, policyTestConfig(t))
		invalid[field] = 0
		if err := ValidateConfig(invalid); err == nil {
			t.Errorf("zero %s was accepted", field)
		}
	}
	config = policyTestConfig(t)
	if _, exists := config["vague_sol_bases"]; exists {
		t.Fatal("obsolete vague_sol_bases setting remains")
	}
	if err := ValidateConfig(config); err != nil {
		t.Fatalf("config without vague_sol_bases rejected: %v", err)
	}
}

func TestPythonOrchAssertions(t *testing.T) {
	config := policyTestConfig(t)
	orch := func(overrides map[string]string) string {
		values := map[string]string{"task": "task-1", "phase": "implementation", "route": "normal", "reason": "new_task", "basis": "specific evidence", "scope": ".claude/hooks/orchestrator"}
		for key, value := range overrides {
			if value == "\x00" {
				delete(values, key)
			} else {
				values[key] = value
			}
		}
		line := "ORCH"
		for _, key := range []string{"task", "phase", "route", "reason", "basis", "scope"} {
			if value, ok := values[key]; ok {
				line += "|" + key + "=" + value
			}
		}
		for key, value := range values {
			if key != "task" && key != "phase" && key != "route" && key != "reason" && key != "basis" && key != "scope" {
				line += "|" + key + "=" + value
			}
		}
		return line
	}
	valid := ParseOrchPrompt(orch(nil)+"\nDo the work.", config)
	if !valid.Valid || valid.Code != "VALID" || valid.TaskID == nil || *valid.TaskID != "task-1" || valid.Prompt != "Do the work." || len(valid.Scope) != 1 || valid.Scope[0] != ".claude/hooks/orchestrator" {
		t.Fatalf("valid ORCH parse = %#v", valid)
	}
	plan := ParseOrchPrompt(orch(map[string]string{"phase": "plan", "route": "plan_standard", "scope": "src, docs"})+"\nPlan.", config)
	if !plan.Valid || len(plan.Scope) != 2 || plan.Scope[0] != "docs" || plan.Scope[1] != "src" {
		t.Fatalf("plan parse/scope = %#v", plan)
	}
	if got := ResolveRoute(*plan.Route, config, false); !got.Valid || *got.Model != "gpt-6.1-sol" || *got.Effort != "high" {
		t.Fatalf("plan route = %#v", got)
	}
	for _, item := range [][2]string{{"plan", "normal"}, {"implementation", "plan_standard"}, {"operation", "plan_standard"}} {
		got := ParseOrchPrompt(orch(map[string]string{"phase": item[0], "route": item[1]}), config)
		if got.Valid || !strings.Contains(got.Message, "phase=plan") || !strings.Contains(got.Message, "route=plan_standard") {
			t.Errorf("invalid phase/route pairing %v = %#v", item, got)
		}
	}
	if got := ParseOrchPrompt(orch(map[string]string{"phase": "plan", "route": "plan_standard", "scope": "."}), config); got.Valid {
		t.Fatal("whole-repository plan scope was accepted")
	}

	failures := []struct{ prompt, code string }{
		{"Do the work.", "MISSING_ORCH_LINE"}, {"ORCH task=task-1", "MALFORMED_ORCH_LINE"},
		{orch(map[string]string{"extra": "x"}), "UNKNOWN_KEY"}, {orch(map[string]string{"task": "\x00"}), "MISSING_REQUIRED_KEY"},
		{orch(map[string]string{"phase": "\x00"}), "MISSING_REQUIRED_KEY"}, {orch(map[string]string{"route": "\x00"}), "MISSING_REQUIRED_KEY"},
		{orch(map[string]string{"scope": "\x00"}), "MISSING_REQUIRED_KEY"}, {orch(map[string]string{"phase": "not-a-phase"}), "INVALID_ENUM_VALUE"},
		{orch(map[string]string{"route": "not-a-route"}), "INVALID_ENUM_VALUE"},
	}
	for _, tc := range failures {
		got := ParseOrchPrompt(tc.prompt+"\nbody", config)
		wantPrompt := "body"
		if tc.code == "MISSING_ORCH_LINE" {
			wantPrompt = tc.prompt + "\nbody"
		}
		if got.Valid || got.Code != tc.code || got.Message == "" || got.Prompt != wantPrompt {
			t.Errorf("ParseOrchPrompt(%q) = %#v, expected code %s", tc.prompt, got, tc.code)
		}
	}
	for _, prompt := range []string{"work", "ORCH task=task-1"} {
		got := ParseOrchPrompt(prompt, config)
		if got.Valid || got.Route != nil || ResolveRoute(got.Route, config, false).Valid {
			t.Errorf("invalid ORCH resolved route for %q: %#v", prompt, got)
		}
	}

	for _, tc := range []struct{ scope, code string }{{"\x00", "MISSING_REQUIRED_KEY"}, {".", "UNBOUNDED_SCOPE"}, {"src/../state", "INVALID_SCOPE"}, {"C:\\repo\\src", "INVALID_SCOPE"}} {
		got := ParseOrchPrompt(orch(map[string]string{"scope": tc.scope}), config)
		if got.Code != tc.code {
			t.Errorf("scope %q code = %s, want %s", tc.scope, got.Code, tc.code)
		}
	}
	validScope := ParseOrchPrompt(orch(map[string]string{"scope": " tests ,src\\module,tests "}), config)
	if !validScope.Valid || len(validScope.Scope) != 2 || validScope.Scope[0] != "src/module" || validScope.Scope[1] != "tests" {
		t.Fatalf("normalized scope = %#v", validScope)
	}
	for _, route := range []string{"normal", "consequential", "escalated"} {
		for _, basis := range []string{"", "   "} {
			if got := ParseOrchPrompt(orch(map[string]string{"route": route, "basis": basis}), config); got.Valid || got.Code != "SOL_ROUTE_NEEDS_BASIS" {
				t.Errorf("blank basis accepted for %s: %#v", route, got)
			}
		}
		if got := ParseOrchPrompt(orch(map[string]string{"route": route, "basis": "design boundary"}), config); !got.Valid || got.Basis == nil || *got.Basis != "design boundary" {
			t.Errorf("basis rejected for %s: %#v", route, got)
		}
	}
	for _, route := range []string{"subtle", "mechanical", "operation"} {
		if got := ParseOrchPrompt(orch(map[string]string{"route": route, "basis": "\x00"}), config); !got.Valid || got.Basis != nil {
			t.Errorf("non-Sol basis behavior for %s: %#v", route, got)
		}
	}
	absent := ParseOrchPrompt(orch(map[string]string{"route": "subtle", "reason": "\x00", "basis": "\x00"}), config)
	supplied := ParseOrchPrompt(orch(map[string]string{"reason": "continue", "basis": "complex"}), config)
	fix := ParseOrchPrompt(orch(map[string]string{"reason": "fix", "basis": "complex"}), config)
	emptyBasis := ParseOrchPrompt(orch(map[string]string{"route": "subtle", "basis": ""}), config)
	invalidReason := ParseOrchPrompt(orch(map[string]string{"reason": "invented"}), config)
	if !absent.Valid || absent.Reason != nil || absent.Basis != nil || !supplied.Valid || supplied.Reason == nil || *supplied.Reason != "continue" || supplied.Basis == nil || *supplied.Basis != "complex" || !fix.Valid || *fix.Reason != "fix" || emptyBasis.Basis == nil || *emptyBasis.Basis != "" || invalidReason.Code != "INVALID_ENUM_VALUE" {
		t.Fatalf("optional reason/basis results: absent=%#v supplied=%#v fix=%#v empty=%#v invalid=%#v", absent, supplied, fix, emptyBasis, invalidReason)
	}
	for _, scope := range []string{"\x00", "."} {
		got := ParseOrchPrompt(orch(map[string]string{"phase": "operation", "scope": scope}), config)
		if !got.Valid || len(got.Scope) != 0 {
			t.Errorf("operation scope %q was not ignored: %#v", scope, got)
		}
	}
}
