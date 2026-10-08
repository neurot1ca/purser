package checkreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"unicode/utf8"
)

func TestPythonOracleParity(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	oracleBytes, err := os.ReadFile(filepath.Join(root, ".claude", "hooks", "orchestrator", "tests", "fixtures", "purser_check_oracle.json"))
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
	if err := os.MkdirAll(filepath.Join(root, ".tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(filepath.Join(root, ".tmp"), "checkreport-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	project, err := os.MkdirTemp(filepath.Join(root, ".tmp"), "checkreport-project-")
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
	if err := os.MkdirAll(filepath.Join(root, ".tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(filepath.Join(root, ".tmp"), "checkreport-runtime-")
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
}

func withoutRuntimeRows(report Report) Report {
	checks := make([]Row, 0, len(report.Checks)-1)
	for _, item := range report.Checks {
		if item.Check != "Runtime" && item.Check != "Python" && item.Check != "Python locator" {
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
