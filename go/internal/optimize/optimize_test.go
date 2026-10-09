package optimize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClaudeUserFilesUseActiveConfigDir(t *testing.T) {
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
			if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"enabledPlugins":{"security-guidance@official":true}}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "plugins", "installed_plugins.json"), []byte(`{"plugins":{"security-guidance@official":[{}]}}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "skills", "security-review"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Replies\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if state := pluginEnabled("security-guidance", home, ""); state == nil || !*state {
				t.Fatalf("pluginEnabled = %v", state)
			}
			if got := installedMatches("please use security guidance review", home); len(got) != 1 || got[0] != "security-guidance@official" {
				t.Fatalf("installedMatches = %v", got)
			}
			if got := localSources("security-review", home, ""); !got["user"] {
				t.Fatalf("localSources = %v", got)
			}
			projects := t.TempDir()
			report, err := Analyze(projects, home, "", 30)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := report["reply_rule_present"].(bool); !got {
				t.Fatalf("reply_rule_present = %v", report["reply_rule_present"])
			}
		})
	}
}

func putJSONL(t *testing.T, path string, records ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
}

func parityUser(cwd, entrypoint, prompt string) map[string]any {
	return map[string]any{
		"type": "user", "cwd": cwd, "entrypoint": entrypoint, "userType": "external",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}},
	}
}

func parityAssistant(cwd, entrypoint, id, command string) map[string]any {
	return map[string]any{
		"type": "assistant", "cwd": cwd, "entrypoint": entrypoint, "timestamp": "2026-10-01T12:00:00Z", "requestId": id,
		"message": map[string]any{
			"id": id, "role": "assistant", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 100},
			"content": []any{map[string]any{"type": "text", "text": "ok"}, map[string]any{"type": "tool_use", "id": id + "-tool", "name": "Bash", "input": map[string]any{"command": command}}},
		},
	}
}

func TestRunMatchesLegacyPythonReportOnFixture(t *testing.T) {
	root := t.TempDir()
	home, repo := filepath.Join(root, "home"), filepath.Join(root, "repo")
	projects := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(filepath.Join(repo, ".claude", "skills", "local"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude", "plugins", "cache", "official", "security-guidance", "1.0"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"enabledPlugins":{"security-guidance@official":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(home, ".claude", "plugins", "cache", "official", "security-guidance", "1.0")
	if err := os.WriteFile(filepath.Join(install, "README.md"), []byte("Automated security review for changes."), 0600); err != nil {
		t.Fatal(err)
	}
	installed := `{"plugins":{"security-guidance@official":[{"installPath":"` + strings.ReplaceAll(install, `\`, `\\`) + `"}]}}`
	if err := os.WriteFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), []byte(installed), 0600); err != nil {
		t.Fatal(err)
	}

	listingContent := "- security-guidance:unused: " + strings.Repeat("x", 1400) + "\n- local: local skill\n- plugin:local: plugin copy\n"
	listing := map[string]any{"type": "attachment", "cwd": repo, "entrypoint": "cli", "timestamp": "2026-10-01T12:00:00Z", "attachment": map[string]any{
		"type": "skill_listing", "content": listingContent, "names": []any{"security-guidance:unused", "local", "plugin:local"}, "isInitial": true}}
	mcp := map[string]any{"type": "attachment", "cwd": repo, "entrypoint": "cli", "timestamp": "2026-10-01T12:00:00Z", "attachment": map[string]any{
		"type": "mcp_instructions_delta", "addedNames": []any{"search-server"}, "addedBlocks": []any{"## search-server\n" + strings.Repeat("instructions ", 100)}}}
	commands := []string{"git add -A", "git commit -m message", "claude plugin marketplace list"}
	for sessionIndex := 0; sessionIndex < 3; sessionIndex++ {
		records := []map[string]any{listing, mcp, parityUser(repo, "cli", "work")}
		for commandIndex, command := range commands {
			records = append(records, parityAssistant(repo, "cli", fmt.Sprintf("s%d-%d", sessionIndex, commandIndex), command))
		}
		path := filepath.Join(projects, "project", fmt.Sprintf("session-%d.jsonl", sessionIndex))
		putJSONL(t, path, records...)
		stamp := time.Now().Add(time.Duration(sessionIndex) * time.Second)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	hidden := []map[string]any{parityUser("", "sdk-py", "Run security review for sdk-py"), {"type": "assistant", "entrypoint": "sdk-py", "timestamp": "2026-10-02T12:00:00Z", "message": map[string]any{"id": "hidden", "role": "assistant", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 300}, "content": []any{map[string]any{"type": "text", "text": "ok"}}}}}
	hiddenPath := filepath.Join(projects, "project", "hidden.jsonl")
	putJSONL(t, hiddenPath, hidden...)
	if err := os.Chtimes(hiddenPath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	var goOut bytes.Buffer
	if err := Run(&goOut, projects, home, repo, 30); err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(workspaceRoot, ".claude", "hooks", "orchestrator", "purser_optimize.py")
	cmd := exec.Command(testutil.Python(t), script, "--projects-root", projects, "--days", "30")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "HOMEDRIVE=", "HOMEPATH=")
	pyOut, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("python report failed: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		pyOut = bytes.ReplaceAll(pyOut, []byte("\r\n"), []byte("\n"))
	}
	if got, want := goOut.String(), string(pyOut); got != want {
		t.Fatalf("Go/Python optimize reports differ:\n--- Go ---\n%s\n--- Python ---\n%s", got, want)
	}
}

func TestRunScopesSessionStateAndKeepsConnectorRegistrationIndependent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	other := filepath.Join(root, "other")
	projects := filepath.Join(home, ".claude", "projects")
	for _, p := range []string{home, repo, other} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"enabledPlugins":{"design@synced":false,"ambiguous@one":true,"ambiguous@two":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	connector := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(connector, []byte(`{"mcpServers":{"design":{"command":"independent"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	current := map[string]any{"cwd": repo, "timestamp": "2026-10-01T12:00:00Z", "type": "attachment", "attachment": map[string]any{"type": "skill_listing", "content": "- design:unused: " + strings.Repeat("x", 400) + "\n- ambiguous:unused: " + strings.Repeat("y", 400) + "\n", "names": []any{"design:unused", "ambiguous:unused"}}}
	request := map[string]any{"cwd": repo, "timestamp": "2026-10-01T12:00:01Z", "type": "assistant", "message": map[string]any{"id": "m1", "role": "assistant", "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 100, "output_tokens": 10}, "content": []any{map[string]any{"type": "text", "text": "ok"}}}}
	foreign := map[string]any{"cwd": other, "timestamp": "2026-10-07T12:00:00Z", "type": "attachment", "attachment": map[string]any{"type": "skill_listing", "content": "- design: foreign project evidence", "names": []any{"design:foreign"}}}
	putJSONL(t, filepath.Join(projects, "p", "current.jsonl"), current, request)
	putJSONL(t, filepath.Join(projects, "q", "foreign.jsonl"), foreign)
	before, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(&out, projects, home, repo, 30); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "stopped: plugin disabled · last seen 2026-10-01") || !strings.Contains(got, "disabled in the current configuration but was loaded") {
		t.Fatalf("disabled and scoped evidence missing:\n%s", got)
	}
	if strings.Contains(got, "foreign project evidence") {
		t.Fatalf("foreign project evidence leaked:\n%s", got)
	}
	if !strings.Contains(got, "design connector") || !strings.Contains(got, "Review connector settings") {
		t.Fatalf("independent connector registration missing:\n%s", got)
	}
	if !strings.Contains(got, "ambiguous plugin") || !strings.Contains(got, "unknown · last seen") {
		t.Fatalf("ambiguous marketplace identity was collapsed:\n%s", got)
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("optimize changed plugin configuration")
	}
	if !strings.Contains(got, "Compaction advice:") || !strings.Contains(got, "Median final reply:") || !strings.Contains(got, "System prompts and eager tool schemas are not recorded") {
		t.Fatalf("restored diagnostics missing:\n%s", got)
	}
	if !strings.Contains(got, "/ 500 msg") {
		t.Fatalf("weighted transcript accounting missing:\n%s", got)
	}
}
