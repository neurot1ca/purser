package initsetup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/policy"
	"purser-hook/internal/profile"
)

func TestReviewerListInstalledOnly(t *testing.T) {
	detected := map[string]any{"agents": map[string]any{
		"codex":    map[string]any{"available": true, "models": []any{"gpt-6-luna"}},
		"claude":   map[string]any{"available": false},
		"agy":      map[string]any{"available": true},
		"opencode": map[string]any{"available": true, "models": []any{"deepseek/deepseek-flash", "deepseek/deepseek-v4-pro", "openrouter/deepseek/deepseek-v4-pro", "openai/gpt-6-luna"}},
	}}
	choices := reviewerChoices(detected)
	if len(choices) != 4 || choices[0]["agent"] != "codex" || choices[1]["agent"] != "agy" || choices[2]["agent"] != "opencode" || choices[2]["model"] != "deepseek/deepseek-v4-pro" || choices[3]["agent"] != "none" {
		t.Fatalf("choices = %#v", choices)
	}
	detected["agents"].(map[string]any)["opencode"] = map[string]any{"available": true, "models": []any{"openai/gpt-6-luna"}}
	choices = reviewerChoices(detected)
	if len(choices) != 4 || choices[2]["agent"] != "opencode" || choices[2]["model"] != "openai/gpt-6-luna" {
		t.Fatalf("non-DeepSeek choices = %#v", choices)
	}

	detected["agents"].(map[string]any)["opencode"] = map[string]any{"available": true, "models": []any{"openrouter/deepseek/deepseek-v3-pro", "openrouter/deepseek/deepseek-v4-pro"}}
	choices = reviewerChoices(detected)
	if len(choices) != 4 || choices[2]["agent"] != "opencode" || choices[2]["model"] != "openrouter/deepseek/deepseek-v4-pro" {
		t.Fatalf("fallback choices = %#v", choices)
	}
}

func TestReviewerCurrentDefaultUsesNextInstalledAgent(t *testing.T) {
	got, err := reviewerCurrent(filepath.Join(t.TempDir(), "missing.toml"), map[string]any{"agents": map[string]any{
		"agy":    map[string]any{"available": false},
		"claude": map[string]any{"available": true},
		"codex":  map[string]any{"available": true},
	}})
	if err != nil || got["agent"] != "claude" || got["model"] != "claude-haiku-5-5" || got["effort"] != "medium" {
		t.Fatalf("default reviewer = %#v, err=%v", got, err)
	}
}

func TestReviewerCurrentDefaultNoneInstalled(t *testing.T) {
	got, err := reviewerCurrent(filepath.Join(t.TempDir(), "missing.toml"), map[string]any{"agents": map[string]any{
		"agy": map[string]any{"available": false}, "claude": map[string]any{"available": false}, "codex": map[string]any{"available": false},
	}})
	if err != nil || len(got) != 1 || got["agent"] != "none" {
		t.Fatalf("default reviewer = %#v, err=%v", got, err)
	}
}

func TestReviewerCurrentExplicitSelectionUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`[roles]
reviewer = "agy"

[reviewer]
selected = { agent = "codex", model = "gpt-6-luna", effort = "high" }
`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := reviewerCurrent(path, map[string]any{"agents": map[string]any{"agy": map[string]any{"available": false}}})
	if err != nil || got["agent"] != "codex" || got["model"] != "gpt-6-luna" || got["effort"] != "high" {
		t.Fatalf("explicit reviewer = %#v, err=%v", got, err)
	}
}

func TestReviewerSetAcceptsListedAGYModel(t *testing.T) {
	choices := reviewerChoices(map[string]any{"agents": map[string]any{
		"agy": map[string]any{"available": true},
	}})
	offered := choices[0]["model"].(string)
	listing := []map[string]any{{
		"id":       "gemini-3.8-flash",
		"variants": map[string]string{"high": offered},
	}}
	if !reviewerModelListed("agy", listing, offered) {
		t.Fatalf("listed model %q was not accepted", offered)
	}
}

func TestReviewerSetRefusesAGYModelAbsentFromDiscovery(t *testing.T) {
	listing := []map[string]any{{
		"id":       "gemini-3.8-flash",
		"variants": map[string]string{"medium": "gemini-3.8-flash-medium"},
	}}
	if reviewerModelListed("agy", listing, agyReviewerModel()) {
		t.Fatalf("undiscovered model %q was accepted", agyReviewerModel())
	}
}

func TestReviewerSetWritesSelectionAndPreservesCoder(t *testing.T) {
	text := "schema_version = 1\n\n[roles]\ncoder = \"codex\"\nreviewer = \"agy\"\n\n[coding]\ncoder_agent = \"codex\"\nreviewer1_agent = \"claude\"\n"
	values, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err := editReviewerConfig(&text, values, "codex", "gpt-6-luna", "medium", "\n"); err != nil {
		t.Fatal(err)
	}
	parsed, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	roles := parsed["roles"].(map[string]any)
	selected := parsed["reviewer"].(map[string]any)["selected"].(map[string]any)
	coding := parsed["coding"].(map[string]any)
	if roles["coder"] != "codex" || roles["reviewer"] != "codex" || selected["agent"] != "codex" || selected["model"] != "gpt-6-luna" || selected["effort"] != "medium" || coding["coder_agent"] != "codex" {
		t.Fatalf("parsed config = %#v", parsed)
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := policy.LoadCoderConfig(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded["reviewer1_agent"] != "codex" || loaded["reviewer1_model_requested"] != "gpt-6-luna" || loaded["reviewer1_effort"] != "medium" {
		t.Fatalf("reviewer policy = %#v", loaded)
	}
}

func TestReviewerSetWithoutModelWritesRoleOnly(t *testing.T) {
	text := "schema_version = 1\n\n[roles]\ncoder = \"claude\"\nreviewer = \"agy\"\n\n[reviewer]\nselected = { agent = \"agy\", model = \"gemini-3.8-flash-high\" }\nexecutable = \"old-reviewer\"\n\n[coding]\ncoder_agent = \"claude\"\nreviewer1_agent = \"agy\"\nreviewer1_model_requested = \"gemini-3.8-flash-high\"\nreviewer1_effort = \"old\"\nreviewer1_executable = \"old-reviewer\"\n"
	values, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err := editReviewerConfig(&text, values, "claude", "", "", "\n"); err != nil {
		t.Fatal(err)
	}
	parsed, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	roles := parsed["roles"].(map[string]any)
	if roles["coder"] != "claude" || roles["reviewer"] != "claude" {
		t.Fatalf("roles = %#v", roles)
	}
	reviewer := parsed["reviewer"].(map[string]any)
	if reviewer["selected"] != nil || reviewer["executable"] != nil {
		t.Fatalf("reviewer config = %#v", reviewer)
	}
	coding := parsed["coding"].(map[string]any)
	if coding["coder_agent"] != "claude" || coding["reviewer1_agent"] != nil || coding["reviewer1_model_requested"] != nil || coding["reviewer1_effort"] != nil || coding["reviewer1_executable"] != nil {
		t.Fatalf("coding config = %#v", coding)
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.LoadCoderConfig(root, configPath); err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
}

func TestReviewerSetNone(t *testing.T) {
	text := "schema_version = 1\n\n[roles]\ncoder = \"codex\"\nreviewer = \"claude\"\n\n[reviewer]\nselected = { agent = \"claude\", model = \"claude-haiku-5-5\", effort = \"medium\" }\n"
	values, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err := editReviewerConfig(&text, values, "none", "", "", "\n"); err != nil {
		t.Fatal(err)
	}
	parsed, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	roles := parsed["roles"].(map[string]any)
	if roles["reviewer"] != "none" || parsed["reviewer"].(map[string]any)["selected"] != nil || roles["coder"] != "codex" {
		t.Fatalf("parsed config = %#v", parsed)
	}
}

func TestReviewerSetRefusesMissingAgent(t *testing.T) {
	err := validateReviewerInstalled(map[string]any{"agents": map[string]any{"agy": map[string]any{"available": false}}}, "agy", "gemini-3.8-flash-high")
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("error = %v", err)
	}
}

func TestReviewerAcceptsAnyInstalledOpenCodeModel(t *testing.T) {
	detected := map[string]any{"agents": map[string]any{
		"opencode": map[string]any{"available": true, "models": []any{"openai/gpt-6-luna"}},
	}}
	if err := validateReviewerInstalled(detected, "opencode", "openai/gpt-6-luna"); err != nil {
		t.Fatalf("non-DeepSeek OpenCode model was rejected: %v", err)
	}
	if err := validateReviewerInstalled(detected, "mystery", "openai/gpt-6-luna"); err == nil || !strings.Contains(err.Error(), "unsupported reviewer agent") {
		t.Fatalf("unknown reviewer agent error = %v", err)
	}
}

func TestProjectReviewerSetKeepsCoderAndDisablesGlobalReviewer(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	globalPath := filepath.Join(home, ".orchestrator", "config.toml")
	global := []byte("schema_version = 1\n\n[roles]\ncoder = \"codex\"\nreviewer = \"agy\"\n\n[coding]\ncoder_agent = \"codex\"\nroutes = { normal = { model = \"gpt-6-luna\", effort = \"medium\" } }\n")
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalPath, global, 0o600); err != nil {
		t.Fatal(err)
	}
	var acknowledgement strings.Builder
	if code := ReviewerRun(&acknowledgement, &strings.Builder{}, []string{"set", "--cwd", root, "--agent", "none", "--json"}); code != 0 {
		t.Fatal("reviewer set failed")
	}
	var ack map[string]any
	if err := json.Unmarshal([]byte(acknowledgement.String()), &ack); err != nil || ack["scope"] != "project" || ack["roles_path"] != profile.ProjectRolesPath(state, root) {
		t.Fatalf("acknowledgement = %s, error = %v", acknowledgement.String(), err)
	}
	if after, err := os.ReadFile(globalPath); err != nil || !bytes.Equal(after, global) {
		t.Fatalf("global config = %q, error = %v; want original bytes", after, err)
	}
	effective, err := profile.ResolveUserConfig(state, root, globalPath)
	if err != nil {
		t.Fatal(err)
	}
	roles := effective["roles"].(map[string]any)
	coding := effective["coding"].(map[string]any)
	if roles["reviewer"] != "none" || roles["coder"] != "codex" || coding["coder_agent"] != "codex" || coding["routes"] == nil {
		t.Fatalf("effective roles/coding = %#v / %#v", roles, coding)
	}
}
