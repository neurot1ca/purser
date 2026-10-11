package checkreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"purser-hook/internal/profile"
)

func TestReportFixtures(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	oracleBytes, err := os.ReadFile(filepath.Join(filepath.Dir(source), "testdata", "purser_check_oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oracle map[string]struct {
		Go struct {
			Report   Report            `json:"report"`
			Rendered map[string]string `json:"rendered"`
		} `json:"go"`
	}
	if err := json.Unmarshal(oracleBytes, &oracle); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(t.TempDir(), "checkreport-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	project, err := os.MkdirTemp(t.TempDir(), "checkreport-project-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(project)
	priorLookPath, priorRun, priorRegistration := lookPath, runCodex, checkRegistration
	defer func() { lookPath, runCodex, checkRegistration = priorLookPath, priorRun, priorRegistration }()
	priorCodexHome, hadCodexHome := os.LookupEnv("CODEX_HOME")
	t.Cleanup(func() {
		if hadCodexHome {
			_ = os.Setenv("CODEX_HOME", priorCodexHome)
		} else {
			_ = os.Unsetenv("CODEX_HOME")
		}
	})
	_ = os.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	writeSettings := func(manual, plugin bool) {
		payload := map[string]any{"hooks": map[string]any{}}
		if manual {
			payload["hooks"] = map[string]any{"PreToolUse": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "python ~/.orchestrator/hook.py"}}}}}
		}
		if plugin {
			payload["enabledPlugins"] = map[string]any{"purser@purser-local": true}
		}
		data, _ := json.Marshal(payload)
		if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"healthy_install", "missing_registration", "registration_conflict", "missing_codex", "internal_inspection_failure"} {
		t.Run(name, func(t *testing.T) {
			writeSettings(false, true)
			lookPath = func(string) (string, error) { return "C:/fake/codex.exe", nil }
			runCodex = func(string) (string, string, int, error) { return "codex 0.156.0", "", 0, nil }
			checkRegistration = registrationCheck
			switch name {
			case "missing_registration":
				writeSettings(false, false)
			case "registration_conflict":
				writeSettings(true, true)
			case "missing_codex":
				lookPath = func(string) (string, error) { return "", errors.New("missing") }
			case "internal_inspection_failure":
				checkRegistration = func(string) Row { panic("broken inspection") }
			}
			got := run(project, root, home)
			got = withoutRuntimeRows(got)
			want := oracle[name].Go.Report
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("report differs from Python oracle\n got: %#v\nwant: %#v", got, want)
			}
			if markdown := RenderMarkdown(got); markdown != oracle[name].Go.Rendered["markdown"] {
				t.Fatalf("Markdown differs from Python oracle\n got: %s\nwant: %s", markdown, oracle[name].Go.Rendered["markdown"])
			}
			var output bytes.Buffer
			if err := WriteJSON(&output, got); err != nil {
				t.Fatal(err)
			}
			if output.String() != oracle[name].Go.Rendered["json"]+"\n" {
				t.Fatalf("JSON differs from Python oracle\n got: %s\nwant: %s", output.String(), oracle[name].Go.Rendered["json"]+"\n")
			}
		})
	}
}

func TestRuntimeRowUsesPluginVersionAndPlatform(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	home, err := os.MkdirTemp(t.TempDir(), "checkreport-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", "settings.json"), `{"enabledPlugins":{"purser@local":true}}`)
	priorLookPath, priorRegistration := lookPath, checkRegistration
	lookPath = func(string) (string, error) { return "", errors.New("missing") }
	checkRegistration = registrationCheck
	defer func() { lookPath, checkRegistration = priorLookPath, priorRegistration }()
	report := run(root, root, home)
	got := report.Checks[0]
	want := "purser-hook " + pluginVersion(root) + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if got.Check != "Runtime" || got.Status != "OK" || got.Detail != want || got.NextStep != nil {
		t.Fatalf("Runtime row = %#v; want Runtime/OK/%q/no next step", got, want)
	}
}

func TestWriteJSONPreservesUTF8(t *testing.T) {
	report := Report{Checks: []Row{row("Unicode", "WARN", "Turkish ı and marker ❯", "")}, NextSteps: []NextStep{}}
	var output bytes.Buffer
	if err := WriteJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(output.Bytes()) || !bytes.Contains(output.Bytes(), []byte("Turkish ı and marker ❯")) {
		t.Fatalf("JSON was not emitted as UTF-8: %q", output.Bytes())
	}
	var parsed Report
	if err := json.Unmarshal(output.Bytes(), &parsed); err != nil || parsed.Checks[0].Detail != report.Checks[0].Detail {
		t.Fatalf("JSON output did not round-trip: report=%#v error=%v", parsed, err)
	}
}

func TestManualOnlyRegistrationIsOK(t *testing.T) {
	home := checkReportTestDir(t, "checkreport-manual-registration-")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		t.Fatal(err)
	}
	settings := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"python ~/.orchestrator/hook.py"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	got := registrationCheck(home)
	if got.Status != "OK" || !strings.Contains(strings.ToLower(got.Detail), "manual") {
		t.Fatalf("registrationCheck = %#v; want manual-only registration OK", got)
	}
}

func TestDisabledReviewerUsesFullReview(t *testing.T) {
	p := profile.ResolvedProfile{Roles: profile.Roles{Reviewer: "none"}}
	roles := rolesCheck(p)
	if roles.Status != "INFO" || !strings.Contains(roles.Detail, "reviewer=none") || !strings.Contains(roles.Detail, "every call is FULL review") {
		t.Fatalf("rolesCheck = %#v", roles)
	}
	reviewer := reviewerCheck(p, t.TempDir())
	if reviewer.Status != "INFO" || !strings.Contains(reviewer.Detail, "every call is FULL review") {
		t.Fatalf("reviewerCheck = %#v", reviewer)
	}
}

func TestMissingReviewerExecutableWarnsAndUsesFullReview(t *testing.T) {
	pluginRoot := t.TempDir()
	configPath := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing-reviewer.exe")
	config, err := json.Marshal(map[string]string{"reviewer1_executable": missing})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	p := profile.ResolvedProfile{Roles: profile.Roles{Reviewer: "agy"}}
	got := reviewerCheck(p, pluginRoot)
	if got.Status != "WARN" || !strings.Contains(got.Detail, "reviewer unavailable") || !strings.Contains(got.Detail, "every call will be FULL review") {
		t.Fatalf("reviewerCheck = %#v", got)
	}
}

func TestReviewerOnPathWithoutExplicitExecutableIsOK(t *testing.T) {
	pluginRoot := t.TempDir()
	configPath := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(pathDir, name), []byte("fake reviewer"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	p := profile.ResolvedProfile{Roles: profile.Roles{Reviewer: "claude"}}
	got := reviewerCheck(p, pluginRoot)
	if got.Status != "OK" || !strings.Contains(got.Detail, name) || got.NextStep != nil {
		t.Fatalf("reviewerCheck = %#v; want PATH-resolved claude reviewer OK", got)
	}
}

func TestReviewerWithoutExplicitExecutableOrPathWarns(t *testing.T) {
	pluginRoot := t.TempDir()
	configPath := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	p := profile.ResolvedProfile{Roles: profile.Roles{Reviewer: "claude"}}
	got := reviewerCheck(p, pluginRoot)
	if got.Status != "WARN" || !strings.Contains(got.Detail, "reviewer unavailable") || !strings.Contains(got.Detail, "every call will be FULL review") || got.NextStep == nil {
		t.Fatalf("reviewerCheck = %#v; want unavailable reviewer WARN with FULL review", got)
	}
}

func TestHookActivityRowMissingFile(t *testing.T) {
	home := checkReportTestDir(t, "checkreport-activity-missing-")
	t.Setenv("PURSER_STATE_DIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	got := hookActivityCheck(home)
	if got.Check != "Hook activity" || got.Status != "INFO" || got.Detail != "No hook activity recorded yet." || got.NextStep != nil {
		t.Fatalf("Hook activity row = %#v", got)
	}
}

func TestHookActivityRowPopulatedFile(t *testing.T) {
	home := checkReportTestDir(t, "checkreport-activity-populated-")
	t.Setenv("PURSER_STATE_DIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	stateRoot := filepath.Join(home, ".claude", "purser")
	if err := os.MkdirAll(stateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	activity := map[string]string{
		"PreToolUse": time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		"Stop":       time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(activity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "hook-activity.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	got := hookActivityCheck(home)
	if got.Check != "Hook activity" || got.Status != "OK" || got.NextStep != nil ||
		!strings.Contains(got.Detail, "PreToolUse 2h ago") || !strings.Contains(got.Detail, "Stop 1d ago") ||
		!strings.Contains(got.Detail, "updated at most hourly") {
		t.Fatalf("Hook activity row = %#v", got)
	}
}

func TestRegistrationUsesActiveClaudeConfigDir(t *testing.T) {
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
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"enabledPlugins":{"purser@local":true}}`), 0600); err != nil {
				t.Fatal(err)
			}
			got := registrationCheck(home)
			if got.Status != "OK" || !strings.Contains(got.Detail, "purser@local") {
				t.Fatalf("registrationCheck = %#v", got)
			}
		})
	}
}

func withoutRuntimeRows(report Report) Report {
	checks := make([]Row, 0, len(report.Checks)-1)
	for _, item := range report.Checks {
		if item.Check != "Runtime" && item.Check != "Python" && item.Check != "Python locator" && item.Check != "Hook activity" {
			checks = append(checks, item)
		}
	}
	report.Checks = checks
	report.NextSteps = []NextStep{}
	report.ExitCode = 0
	for _, item := range checks {
		if item.Status == "WARN" || item.Status == "FAIL" {
			report.NextSteps = append(report.NextSteps, NextStep{Number: len(report.NextSteps) + 1, Check: item.Check, Action: *item.NextStep})
		}
		if item.Status == "FAIL" {
			report.ExitCode = 1
		}
	}
	return report
}

func checkReportTestDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
