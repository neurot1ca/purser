package initsetup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"purser-hook/internal/contextio"
	"purser-hook/internal/policy"
	"purser-hook/internal/profile"
)

func TestCursorSetupDiscoveryUsesAgentCLIAndListedChoices(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	bin := t.TempDir()
	writeFakeCursorCLI(t, bin, "cursor-agent", "Available models\ngpt-5.6-codex-high - GPT\nclaude-opus-5-5-high - Claude\ngemini-3.8-flash-high - Gemini\ngrok-4.7-high - Grok\ncomposer-2.5 - Composer\nauto - Automatic\n")
	t.Setenv("PATH", bin)
	got, err := detect(home, root, options{})
	if err != nil {
		t.Fatal(err)
	}
	agents := got["agents"].(map[string]any)
	cursorAgent, ok := agents["cursor"].(map[string]any)
	if !ok {
		t.Fatalf("Cursor CLI not detected: %#v", agents["cursor"])
	}
	if cursorAgent["available"] != true {
		t.Fatalf("Cursor CLI not detected: %#v", cursorAgent)
	}
	models := cursorAgent["models"].([]any)
	if !containsStringValue(models, "gpt-5.6-codex-high") || containsStringValue(models, "auto") {
		t.Fatalf("Cursor models = %#v", models)
	}
	options := cursorAgent["options"].([]any)
	labels := []any{}
	for _, item := range options {
		labels = append(labels, item.(map[string]any)["label"])
	}
	if !containsStringValue(models, "claude-opus-5-5-high") || len(labels) != 4 || !containsStringValue(labels, "latest:gpt-codex-high") {
		t.Fatalf("Cursor options = %#v", options)
	}
	for _, label := range labels {
		if label == "auto" || !strings.HasPrefix(label.(string), "latest:") {
			t.Fatalf("Cursor option was not a family selector: %#v", options)
		}
	}
}

func TestCursorSetupDoesNotTreatEditorLauncherAsAgentCLI(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	bin := t.TempDir()
	writeFakeCursorCLI(t, bin, "cursor", "Available models\ngpt-5.6-sol-high - GPT\n")
	t.Setenv("PATH", bin)
	got, err := detect(home, root, options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["agents"].(map[string]any)["cursor"]; present {
		t.Fatalf("editor launcher enabled Cursor: %#v", got["agents"])
	}
}

func TestCursorSetupListingFailureIsUnavailable(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	bin := t.TempDir()
	writeFakeCursorCLI(t, bin, "cursor-agent", "")
	t.Setenv("PATH", bin)
	got, err := detect(home, root, options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["agents"].(map[string]any)["cursor"]; present {
		t.Fatalf("failed Cursor listing was shown: %#v", got["agents"])
	}
	if len(got["reviewer_options"].([]map[string]any)) != 0 {
		t.Fatalf("failed Cursor listing offered reviewer choices: %#v", got["reviewer_options"])
	}
}

func writeFakeCursorCLI(t *testing.T, dir, name, output string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		path += ".cmd"
		body := "@echo off\r\nif \"%1\"==\"models\" (\r\necho " + strings.ReplaceAll(output, "\n", "\necho ") + "\r\n)\r\n"
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	body := "#!/bin/sh\nprintf '%s' " + shellQuote(output) + "\n"
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func TestClaudeCoderPlanAndApply(t *testing.T) {
	shippedConfig, err := filepath.Abs(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	home, root, restore := initTestEnvironment(t)
	defer restore()
	state := os.Getenv("PURSER_STATE_DIR")
	configPath := filepath.Join(home, ".orchestrator", "config.toml")
	var plan bytes.Buffer
	if code := Run(&plan, &bytes.Buffer{}, []string{"--plan", "--coder", "claude", "--reviewer", "agy"}); code != 0 {
		t.Fatalf("plan failed: %s", plan.String())
	}
	if strings.Contains(strings.ToLower(plan.String()), "python") {
		t.Errorf("plan unexpectedly mentions Python:\n%s", plan.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".orchestrator", "python")); !os.IsNotExist(err) {
		t.Errorf("plan unexpectedly created ~/.orchestrator/python (stat error: %v)", err)
	}
	for _, route := range []string{
		"mechanical -> haiku / none", "subtle -> sonnet / medium", "normal -> sonnet / medium",
		"consequential -> opus / high", "escalated -> opus / xhigh", "operation -> haiku / none",
		"plan_standard -> opus / high",
	} {
		if !strings.Contains(plan.String(), route) {
			t.Errorf("plan does not list %q:\n%s", route, plan.String())
		}
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	prior := "schema_version = 1\n\n[roles]\ncoder = \"codex\"\n\n[coding]\ncoder_agent = \"codex\"\n\n[coding.routes.normal]\nmodel = \"gpt-6.1-sol\"\neffort = \"medium\"\n"
	if err := os.WriteFile(configPath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	pythonPath := filepath.Join(home, ".orchestrator", "python")
	pythonPrior := []byte("user-owned locator\r\nwith exact bytes\n")
	if err := os.WriteFile(pythonPath, pythonPrior, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, []string{"--apply", "--coder", "claude", "--reviewer", "agy"}); code != 0 {
		t.Fatal("apply failed")
	}
	pythonAfter, err := os.ReadFile(pythonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pythonAfter, pythonPrior) {
		t.Errorf("existing ~/.orchestrator/python changed: got %q, want %q", pythonAfter, pythonPrior)
	}
	loaded, err := policy.LoadCoderConfigForRepository(shippedConfig, configPath, root)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if got := loaded["coder_agent"]; got != "claude" {
		t.Errorf("coder_agent = %v, want claude", got)
	}
	rolePath := profile.ProjectRolesPath(state, root)
	raw, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := profile.DecodeTOML(raw)
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := user["roles"].(map[string]any)
	if roles["coder"] != "claude" {
		t.Errorf("roles.coder = %v, want claude", roles["coder"])
	}
	coding, _ := user["coding"].(map[string]any)
	if _, exists := coding["routes"]; exists {
		t.Error("Claude config should use built-in routes without a [coding.routes] table")
	}
}

func TestEditConfigReplacesCodingRoutesSubtable(t *testing.T) {
	text := "[roles]\ncoder = \"codex\"\nreviewer = \"none\"\n\n[coding]\ncoder_agent = \"codex\"\n\n[coding.routes.normal]\nmodel = \"old-model\"\neffort = \"medium\"\n"
	values, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err := editConfig(&text, values, "", options{coder: "agy", model: "gemini-4-argon", reviewer: "none"}, "\n"); err != nil {
		t.Fatal(err)
	}
	parsed, _, err := profile.DecodeTOML([]byte(text))
	if err != nil {
		t.Fatalf("updated config does not parse: %v\n%s", err, text)
	}
	routes := parsed["coding"].(map[string]any)["routes"].(map[string]any)
	if routes["normal"].(map[string]any)["model"] != "gemini-4-argon" || strings.Contains(text, "old-model") {
		t.Fatalf("routes = %#v, config = %s", routes, text)
	}
}

func TestListModelsMarksRouteRecommendations(t *testing.T) {
	listing := discovered{source: "codex debug models", models: []map[string]any{
		{"id": "gpt-6-luna", "efforts": []string{"low", "high"}},
		{"id": "gpt-6.1-sol", "efforts": []string{"medium", "high", "xhigh"}},
		{"id": "gpt-6-astra", "efforts": []string{"high"}},
	}}
	got := modelList(listing, map[string]string{"gpt-6-luna": "high", "gpt-6.1-sol": "medium"})
	models := got["models"].([]map[string]any)
	if models[0]["recommended"] != true || models[0]["recommended_effort"] != "high" {
		t.Errorf("luna recommendation = %#v", models[0])
	}
	if models[1]["recommended"] != true || models[1]["recommended_effort"] != "medium" {
		t.Errorf("sol recommendation = %#v", models[1])
	}
	if models[2]["recommended"] != false || models[2]["recommended_effort"] != nil {
		t.Errorf("astra recommendation = %#v", models[2])
	}
}

func TestDetectAcceptsCodexModelWithoutModeOrEffort(t *testing.T) {
	_, _, restore := initTestEnvironment(t)
	defer restore()
	path := t.TempDir()
	t.Setenv("PATH", path)
	var output, errOutput bytes.Buffer
	if code := Run(&output, &errOutput, []string{"--detect", "--json", "--coder", "codex", "--model", "gpt-6-luna"}); code != 0 {
		t.Fatalf("detect returned %d: %s", code, errOutput.String())
	}
	var detected map[string]any
	if err := json.Unmarshal(output.Bytes(), &detected); err != nil {
		t.Fatalf("invalid detect output %q: %v", output.String(), err)
	}
}

func TestCodexReviewerOptionsUseNewestTierModelsAndOmitMissingTier(t *testing.T) {
	models := []map[string]any{
		{"id": "gpt-6-sol"}, {"id": "gpt-6.1-sol"}, {"id": "gpt-6-luna"}, {"id": "gpt-6.1-luna"}, {"id": "gpt-6-astra"},
	}
	got := codexReviewerOptions(models)
	if len(got) != 2 || got[0]["model"] != "gpt-6.1-sol" || got[0]["effort"] != "low" || got[1]["model"] != "gpt-6.1-luna" || got[1]["effort"] != "medium" {
		t.Fatalf("reviewer options = %#v", got)
	}
	got = codexReviewerOptions(models[:2])
	if len(got) != 1 || got[0]["model"] != "gpt-6.1-sol" || got[0]["effort"] != "low" {
		t.Fatalf("reviewer options without Luna = %#v", got)
	}
}

func TestReviewerEffortFlagWritesConfig(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	bin := t.TempDir()
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	name := "claude"
	if strings.EqualFold(filepath.Ext(os.Args[0]), ".exe") {
		name += ".exe"
	}
	claude := filepath.Join(bin, name)
	if err := os.WriteFile(claude, raw, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	var output, errOutput bytes.Buffer
	args := []string{"--apply", "--coder", "claude", "--reviewer", "claude", "--reviewer-model", "claude-haiku-5-5", "--reviewer-effort", "low"}
	if code := Run(&output, &errOutput, args); code != 0 {
		t.Fatalf("apply returned %d: %s", code, errOutput.String())
	}
	rolePath := profile.ProjectRolesPath(os.Getenv("PURSER_STATE_DIR"), root)
	config, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatal(err)
	}
	values, _, err := profile.DecodeTOML(config)
	if err != nil {
		t.Fatal(err)
	}
	selected := values["reviewer"].(map[string]any)["selected"].(map[string]any)
	if selected["model"] != "claude-haiku-5-5" || selected["effort"] != "low" {
		t.Fatalf("persisted reviewer selection = %#v (home %s)", selected, home)
	}
}

func TestPlanAndApplyRejectInconsistentModelFlagsEqually(t *testing.T) {
	_, _, restore := initTestEnvironment(t)
	defer restore()
	for _, operation := range []string{"--plan", "--apply"} {
		args := []string{operation, "--coder", "codex", "--reviewer", "none", "--mode", "balanced", "--model", "gpt-6-luna"}
		if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, args); code != 2 {
			t.Errorf("%s returned %d for balanced mode with a manual model; want 2", operation, code)
		}
	}
}

func TestPlanAcceptsMaintenanceWithOpenCodeCoder(t *testing.T) {
	_, _, restore := initTestEnvironment(t)
	defer restore()
	var output, errOutput bytes.Buffer
	if code := Run(&output, &errOutput, []string{"--plan", "--coder", "opencode", "--reviewer", "none", "--maintenance"}); code == 2 || strings.Contains(errOutput.String(), "maintenance reports can only be configured") {
		t.Fatalf("--maintenance was rejected for OpenCode: exit %d: %s", code, errOutput.String())
	}
}

func TestApplyInNonGitDirectorySkipsMaintenanceAndWritesConfig(t *testing.T) {
	_, root, restore := initTestEnvironment(t)
	defer restore()
	t.Setenv("PATH", t.TempDir())
	var output, errOutput bytes.Buffer
	args := []string{"--apply", "--json", "--coder", "codex", "--reviewer", "none", "--no-maintenance"}
	if code := Run(&output, &errOutput, args); code != 0 {
		t.Fatalf("apply returned %d: %s", code, errOutput.String())
	}
	rolesPath := profile.ProjectRolesPath(os.Getenv("PURSER_STATE_DIR"), root)
	if raw, err := os.ReadFile(rolesPath); err != nil || !strings.Contains(string(raw), `coder = "codex"`) {
		t.Fatalf("project roles were not written for %s: %q, %v", root, raw, err)
	}
	var applied map[string]any
	if err := json.Unmarshal(output.Bytes(), &applied); err != nil {
		t.Fatalf("invalid apply output %q: %v", output.String(), err)
	}
	skipped, ok := applied["skipped"].([]any)
	if !ok || len(skipped) != 1 || skipped[0] != "maintenance reports: project is not in a Git repository" {
		t.Fatalf("skipped steps = %#v", applied["skipped"])
	}
}

func TestAutoCompactInit(t *testing.T) {
	_, _, restore := initTestEnvironment(t)
	defer restore()
	state := os.Getenv("PURSER_STATE_DIR")
	marker := filepath.Join(state, "auto-compact.json")
	baseArgs := []string{"--coder", "claude", "--reviewer", "agy"}

	for _, tc := range []struct {
		flag string
		want bool
	}{{"--auto-compact", true}, {"--no-auto-compact", false}} {
		t.Run(tc.flag, func(t *testing.T) {
			planArgs := append([]string{"--plan"}, append(baseArgs, tc.flag)...)
			var plan bytes.Buffer
			if code := Run(&plan, &bytes.Buffer{}, planArgs); code != 0 {
				t.Fatalf("plan failed with %d: %s", code, plan.String())
			}
			wantPlan := "Automatic /compact: disabled for this user"
			if tc.want {
				wantPlan = "Automatic /compact: enabled for this user"
			}
			if !strings.Contains(plan.String(), wantPlan) {
				t.Errorf("plan missing %q:\n%s", wantPlan, plan.String())
			}
			applyArgs := append([]string{"--apply"}, append(baseArgs, tc.flag)...)
			if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, applyArgs); code != 0 {
				t.Fatalf("apply failed with %d", code)
			}
			var saved struct {
				Enabled bool `json:"enabled"`
			}
			raw, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("invalid auto-compact marker %q: %v", raw, err)
			}
			if saved.Enabled != tc.want {
				t.Errorf("saved enabled = %t, want %t", saved.Enabled, tc.want)
			}
		})
	}

	original := []byte("{\"enabled\":true}\n")
	if err := os.WriteFile(marker, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, append([]string{"--apply"}, baseArgs...)); code != 0 {
		t.Fatalf("apply without auto-compact choice failed with %d", code)
	}
	after, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("apply without choice changed auto-compact file: got %q, want %q", after, original)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, append(append([]string{"--plan"}, baseArgs...), "--auto-compact", "--no-auto-compact")); code != 2 {
		t.Errorf("conflicting auto-compact flags returned %d, want usage error 2", code)
	}

	for _, want := range []any{nil, true, false} {
		if want != nil {
			raw, _ := json.Marshal(map[string]bool{"enabled": want.(bool)})
			if err := os.WriteFile(marker, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		var output bytes.Buffer
		args := []string{"--detect", "--json"}
		if code := Run(&output, &bytes.Buffer{}, args); code != 0 {
			t.Fatalf("detect failed with %d: %s", code, output.String())
		}
		var detected map[string]any
		if err := json.Unmarshal(output.Bytes(), &detected); err != nil {
			t.Fatalf("invalid detect output %q: %v", output.String(), err)
		}
		if got, ok := detected["auto_compact_enabled"]; !ok || got != want {
			t.Errorf("auto_compact_enabled = %#v (present %t), want %#v", got, ok, want)
		}
	}
}

func TestEnableConciseUsesActiveClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct {
		name, configDir string
	}{
		{name: "explicit", configDir: filepath.Join(t.TempDir(), "claude-config")},
		{name: "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tc.configDir)
			dir := tc.configDir
			if dir == "" {
				dir = filepath.Join(home, ".claude")
			}
			var out, errOut bytes.Buffer
			if code := enableConcise(&out, &errOut); code != 0 {
				t.Fatalf("enableConcise returned %d: %s", code, errOut.String())
			}
			data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(data, &settings); err != nil || settings["outputStyle"] != "Concise" {
				t.Fatalf("settings = %s, error = %v", data, err)
			}
		})
	}
}

func TestPlanDoesNotWriteAndShowsHandoff(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	var out, errOut bytes.Buffer
	args := []string{"--plan", "--coder", "codex", "--reviewer", "none", "--trust", "--scaffold-profile"}
	if code := Run(&out, &errOut, args); code != 0 {
		t.Fatalf("plan returned %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "---") || !strings.Contains(out.String(), "+++") || !strings.Contains(out.String(), "schema_version = 1") {
		t.Fatalf("plan did not contain the expected diff and profile: %s", out.String())
	}
	handoff := filepath.Join(stateRootPath(), "projects", contextio.ProjectKey(mustCwd()), "telemetry", "purser-handoff.md")
	if !strings.Contains(out.String(), "Create "+handoff+":") {
		t.Errorf("plan omitted handoff path %q: %s", handoff, out.String())
	}
	if _, err := os.Stat(handoff); !os.IsNotExist(err) {
		t.Errorf("plan created handoff (stat error: %v)", err)
	}
	for _, path := range []string{filepath.Join(home, ".orchestrator"), filepath.Join(root, ".orchestrator")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("plan created %s (stat error: %v)", path, err)
		}
	}
}

func TestApplyPreservesExistingConfigBytesAndBacksUp(t *testing.T) {
	original := []byte("schema_version = 1\r\n# keep comment\r\n[telemetry]\r\nvault_root = \"C:/vault\" # keep exact\r\n[roles]\r\ncoder = \"claude\"\r\n[trust]\r\npaths = []\r\n")
	home, _, restore := initTestEnvironment(t)
	defer restore()
	config := filepath.Join(home, ".orchestrator", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run(&out, &errOut, []string{"--apply", "--coder", "codex", "--reviewer", "none", "--trust"}); code != 0 {
		t.Fatalf("apply returned %d: %s", code, errOut.String())
	}
	changed, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(changed, []byte("# keep comment\r\n[telemetry]\r\nvault_root = \"C:/vault\" # keep exact\r\n")) {
		t.Errorf("unrelated CRLF bytes or comments changed:\n%s", changed)
	}
	projectRolesPath := profile.ProjectRolesPath(os.Getenv("PURSER_STATE_DIR"), mustCwd())
	projectRoles, err := os.ReadFile(projectRolesPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := profile.DecodeTOML(projectRoles)
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := parsed["roles"].(map[string]any)
	if roles["orchestrator"] != "claude-code" || roles["coder"] != "codex" || roles["reviewer"] != "none" {
		t.Errorf("saved roles = %#v", roles)
	}
	global, _, err := profile.DecodeTOML(changed)
	if err != nil {
		t.Fatal(err)
	}
	trust, _ := global["trust"].(map[string]any)
	paths, _ := trust["paths"].([]any)
	if !containsStringValue(paths, mustCwd()) {
		t.Errorf("project path %q missing from trust paths: %#v", mustCwd(), paths)
	}
	backups, err := filepath.Glob(config + ".bak-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup files = %v, error = %v; want one backup", backups, err)
	}
	backupBytes, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(backupBytes, original) {
		t.Errorf("backup = %q, error = %v; want original bytes", backupBytes, err)
	}
}

func TestInvalidConfigIsUntouchedAndCreatesNoBackup(t *testing.T) {
	home, _, restore := initTestEnvironment(t)
	defer restore()
	config := filepath.Join(home, ".orchestrator", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("schema_version = 1\n[bad\n")
	if err := os.WriteFile(config, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, []string{"--apply", "--coder", "codex", "--reviewer", "none", "--trust"}); code == 0 {
		t.Fatal("apply accepted invalid config")
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(after, original) {
		t.Errorf("config after failed apply = %q, error = %v", after, err)
	}
	backups, err := filepath.Glob(config + ".bak-*")
	if err != nil || len(backups) != 0 {
		t.Errorf("failed apply created backups %v (error %v)", backups, err)
	}
}

func TestSecondApplyIsNoopAndKeepsExistingHandoff(t *testing.T) {
	home, _, restore := initTestEnvironment(t)
	defer restore()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	args := []string{"--apply", "--coder", "claude", "--reviewer", "none", "--trust", "--scaffold-profile"}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, args); code != 0 {
		t.Fatal("first apply failed")
	}
	config := filepath.Join(home, ".orchestrator", "config.toml")
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(mustCwd(), ".orchestrator", "profile.toml")
	profileBytes, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	rolePath := profile.ProjectRolesPath(state, mustCwd())
	roleBytes, err := os.ReadFile(rolePath)
	if err != nil {
		t.Fatal(err)
	}
	handoff := filepath.Join(state, "projects", contextio.ProjectKey(mustCwd()), "telemetry", "purser-handoff.md")
	if err := os.WriteFile(handoff, []byte("user handoff; keep it\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Run(&out, &bytes.Buffer{}, args); code != 0 {
		t.Fatalf("second apply failed: %s", out.String())
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("second apply changed config: %v", err)
	}
	afterProfile, err := os.ReadFile(profilePath)
	if err != nil || !bytes.Equal(afterProfile, profileBytes) {
		t.Errorf("second apply changed profile: %v", err)
	}
	afterRoles, roleErr := os.ReadFile(rolePath)
	if roleErr != nil || !bytes.Equal(afterRoles, roleBytes) {
		t.Errorf("second apply changed project roles: got %q, want %q, error %v", afterRoles, roleBytes, roleErr)
	}
	if !strings.HasPrefix(out.String(), "No changes.\nHandoff: ") {
		t.Errorf("second apply output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".orchestrator", "python")); !os.IsNotExist(err) {
		t.Errorf("apply created Python locator (stat error: %v)", err)
	}
	if got, err := os.ReadFile(handoff); err != nil || string(got) != "user handoff; keep it\n" {
		t.Errorf("existing handoff = %q, error = %v", got, err)
	}
}

func TestTrustOnlyGlobalChangeReportsAppliedThenNoChanges(t *testing.T) {
	_, _, restore := initTestEnvironment(t)
	defer restore()
	args := []string{"--apply", "--coder", "claude", "--reviewer", "none"}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, args); code != 0 {
		t.Fatal("initial apply failed")
	}
	var first bytes.Buffer
	trustArgs := append(args, "--trust")
	if code := Run(&first, &bytes.Buffer{}, trustArgs); code != 0 {
		t.Fatalf("trust apply failed: %s", first.String())
	}
	if !strings.HasPrefix(first.String(), "Applied.\nHandoff: ") {
		t.Fatalf("trust-only apply output = %q", first.String())
	}
	var second bytes.Buffer
	if code := Run(&second, &bytes.Buffer{}, trustArgs); code != 0 {
		t.Fatalf("second trust apply failed: %s", second.String())
	}
	if !strings.HasPrefix(second.String(), "No changes.\nHandoff: ") {
		t.Fatalf("second trust apply output = %q", second.String())
	}
}

func TestRoleChangeOnCompleteProjectRolesReportsAppliedThenNoChanges(t *testing.T) {
	_, root, restore := initTestEnvironment(t)
	defer restore()
	state := os.Getenv("PURSER_STATE_DIR")
	rolePath := profile.ProjectRolesPath(state, root)
	if err := os.MkdirAll(filepath.Dir(rolePath), 0o700); err != nil {
		t.Fatal(err)
	}
	complete := "schema_version = 1\n\n[roles]\norchestrator = \"claude-code\"\ncoder = \"claude\"\nreviewer = \"none\"\n\n[coding]\ncoder_agent = \"claude\"\n"
	if err := os.WriteFile(rolePath, []byte(complete), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--apply", "--coder", "codex", "--reviewer", "none"}
	var first bytes.Buffer
	if code := Run(&first, &bytes.Buffer{}, args); code != 0 {
		t.Fatalf("role change failed: %s", first.String())
	}
	if !strings.HasPrefix(first.String(), "Applied.\nHandoff: ") {
		t.Fatalf("role change output = %q", first.String())
	}
	var second bytes.Buffer
	if code := Run(&second, &bytes.Buffer{}, args); code != 0 {
		t.Fatalf("second role apply failed: %s", second.String())
	}
	if !strings.HasPrefix(second.String(), "No changes.\nHandoff: ") {
		t.Fatalf("second role apply output = %q", second.String())
	}
}

func TestConciseOfferIsClaimedOnce(t *testing.T) {
	home, _, restore := initTestEnvironment(t)
	defer restore()
	args := []string{"--apply", "--coder", "claude", "--reviewer", "none"}
	var first bytes.Buffer
	if code := Run(&first, &bytes.Buffer{}, args); code != 0 {
		t.Fatalf("first apply failed: %s", first.String())
	}
	if !strings.Contains(first.String(), "Concise reply style offer due.") {
		t.Errorf("first apply omitted offer: %s", first.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".orchestrator", "reply-style-suggestion-shown")); err != nil {
		t.Errorf("offer marker missing: %v", err)
	}
	var second bytes.Buffer
	if code := Run(&second, &bytes.Buffer{}, args); code != 0 {
		t.Fatalf("second apply failed: %s", second.String())
	}
	if strings.Contains(second.String(), "Concise reply style offer due.") {
		t.Errorf("second apply repeated offer: %s", second.String())
	}
}

func TestGlobalConciseStyleSuppressesOfferAndProjectStyleTakesPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, global, project string
		wantOffer             bool
	}{
		{name: "global concise", global: `{"outputStyle":"Concise"}`},
		{name: "project override", global: `{"outputStyle":"Concise"}`, project: `{"outputStyle":"Explanatory"}`, wantOffer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, root, restore := initTestEnvironment(t)
			defer restore()
			globalPath := filepath.Join(home, ".claude", "settings.json")
			if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(globalPath, []byte(tc.global), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.project != "" {
				projectPath := filepath.Join(root, ".claude", "settings.json")
				if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(projectPath, []byte(tc.project), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if code := Run(&out, &bytes.Buffer{}, []string{"--apply", "--coder", "claude", "--reviewer", "none"}); code != 0 {
				t.Fatalf("apply failed: %s", out.String())
			}
			gotOffer := strings.Contains(out.String(), "Concise reply style offer due.")
			if gotOffer != tc.wantOffer {
				t.Errorf("offer due = %t, want %t; output: %s", gotOffer, tc.wantOffer, out.String())
			}
			marker := filepath.Join(home, ".orchestrator", "reply-style-suggestion-shown")
			_, err := os.Stat(marker)
			if tc.wantOffer && err != nil {
				t.Errorf("expected offer marker: %v", err)
			}
			if !tc.wantOffer && !os.IsNotExist(err) {
				t.Errorf("global Concise style created marker (stat error: %v)", err)
			}
		})
	}
}

func TestEnableConcisePreservesSettingsAndReportsProjectOverride(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{\n  \"other\": {\"keep\": true},\n  \"outputStyle\": \"Explanatory\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte(`{"outputStyle":"Explanatory"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Run(&out, &bytes.Buffer{}, []string{"--enable-concise-style"}); code != 0 {
		t.Fatalf("enable concise failed: %s", out.String())
	}
	var saved map[string]any
	raw, err := os.ReadFile(settings)
	if err != nil || json.Unmarshal(raw, &saved) != nil {
		t.Fatalf("settings = %s, error = %v", raw, err)
	}
	other, _ := saved["other"].(map[string]any)
	if saved["outputStyle"] != "Concise" || other["keep"] != true {
		t.Errorf("settings update lost or failed to set values: %#v", saved)
	}
	if !strings.Contains(strings.ToLower(out.String()), "project") || !strings.Contains(strings.ToLower(out.String()), "override") {
		t.Errorf("output did not report project override: %s", out.String())
	}
	var unchanged map[string]any
	projectBytes, err := os.ReadFile(shared)
	if err != nil || json.Unmarshal(projectBytes, &unchanged) != nil || unchanged["outputStyle"] != "Explanatory" {
		t.Errorf("shared project settings changed: %s, error %v", projectBytes, err)
	}
}

func TestExistingProfileIsNeverOverwritten(t *testing.T) {
	_, root, restore := initTestEnvironment(t)
	defer restore()
	profilePath := filepath.Join(root, ".orchestrator", "profile.toml")
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("existing content\n")
	if err := os.WriteFile(profilePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(&bytes.Buffer{}, &bytes.Buffer{}, []string{"--apply", "--coder", "codex", "--reviewer", "none", "--scaffold-profile"}); code != 0 {
		t.Fatal("apply failed")
	}
	after, err := os.ReadFile(profilePath)
	if err != nil || !bytes.Equal(after, original) {
		t.Errorf("profile = %q, error = %v; want original bytes", after, err)
	}
}

func TestProjectRoleApplyLeavesGlobalConfigUntouched(t *testing.T) {
	home, root, restore := initTestEnvironment(t)
	defer restore()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	otherRoot := t.TempDir()
	otherRolePath := profile.ProjectRolesPath(state, otherRoot)
	otherRoles := []byte("schema_version = 1\n\n[roles]\ncoder = \"opencode\"\n")
	if err := os.MkdirAll(filepath.Dir(otherRolePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherRolePath, otherRoles, 0o600); err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(home, ".orchestrator", "config.toml")
	global := []byte("schema_version = 1\n\n[roles]\ncoder = \"codex\"\nreviewer = \"agy\"\n\n[coding]\ncoder_agent = \"codex\"\n")
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalPath, global, 0o600); err != nil {
		t.Fatal(err)
	}
	var acknowledgement bytes.Buffer
	if code := Run(&acknowledgement, &bytes.Buffer{}, []string{"--apply", "--coder", "claude", "--reviewer", "none", "--json"}); code != 0 {
		t.Fatal("apply failed")
	}
	var ack map[string]any
	if err := json.Unmarshal(acknowledgement.Bytes(), &ack); err != nil || ack["scope"] != "project" || ack["roles_path"] != profile.ProjectRolesPath(state, root) {
		t.Fatalf("acknowledgement = %s, error = %v", acknowledgement.String(), err)
	}
	if after, err := os.ReadFile(globalPath); err != nil || !bytes.Equal(after, global) {
		t.Fatalf("global config = %q, error = %v; want original bytes", after, err)
	}
	if after, err := os.ReadFile(otherRolePath); err != nil || !bytes.Equal(after, otherRoles) {
		t.Fatalf("other project roles = %q, error = %v; want original bytes", after, err)
	}
	rolePath := profile.ProjectRolesPath(state, root)
	values, err := profile.ResolveUserConfig(state, root, globalPath)
	if err != nil {
		t.Fatal(err)
	}
	roles := values["roles"].(map[string]any)
	if roles["coder"] != "claude" || roles["reviewer"] != "none" {
		t.Fatalf("effective roles = %#v", roles)
	}
	if _, err := os.Stat(rolePath); err != nil {
		t.Fatalf("project roles file was not written: %v", err)
	}
}

func TestProjectRolePathResolvesSymlinkRoot(t *testing.T) {
	state, root := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if got, want := profile.ProjectRolesPath(state, alias), profile.ProjectRolesPath(state, root); got != want {
		t.Fatalf("symlink project roles path = %q, want %q", got, want)
	}
}

func TestProjectRoleConcurrentUpdatesKeepBothSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roles.toml")
	if err := os.WriteFile(path, []byte("schema_version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, section := range []string{"fallback", "mechanical"} {
		section := section
		go func() {
			<-start
			errs <- updateLocked(path, func(raw []byte) ([]byte, error) {
				values, _, err := profile.DecodeTOML(raw)
				if err != nil {
					return nil, err
				}
				values[section] = map[string]any{"enabled": true}
				return encodeRoleSections(values, "fallback", "mechanical")
			})
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values, _, err := profile.DecodeTOML(raw)
	if err != nil {
		t.Fatal(err)
	}
	if values["fallback"] == nil || values["mechanical"] == nil {
		t.Fatalf("concurrent role update lost a section: %#v", values)
	}
}

func initTestEnvironment(t *testing.T) (home, root string, restore func()) {
	t.Helper()
	home, root = t.TempDir(), t.TempDir()
	state := t.TempDir()
	for _, value := range os.Environ() {
		key, _, ok := strings.Cut(value, "=")
		if ok && strings.HasPrefix(key, "PURSER_") {
			t.Setenv(key, "")
		}
	}
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	return home, root, func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}
}

func containsStringValue(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
