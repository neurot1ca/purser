package optimize

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
