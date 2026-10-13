package contextio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionStartLanguageFallback(t *testing.T) {
	tests := []struct {
		name    string
		global  string
		project string
		want    string
		avoid   string
	}{
		{name: "global language", global: "Turkish", want: "Reply to the user in Turkish."},
		{name: "project language wins", global: "Turkish", project: "User language: German\n", want: "Reply to the user in German"},
		{name: "project English suppresses global instruction", global: "Turkish", project: "User language: English\n", avoid: "Reply to the user in Turkish"},
		{name: "no recorded language", want: "Reply in the language the user writes in."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateRoot := t.TempDir()
			t.Setenv("PURSER_STATE_DIR", stateRoot)
			repo := updateSessionTestRepo(t)
			if tt.global != "" {
				RecordUserLanguage(stateRoot, tt.global)
			}
			if tt.project != "" {
				handoff := filepath.Join(stateRoot, "projects", repositoryProjectKey(repo), "telemetry", "purser-handoff.md")
				if err := os.MkdirAll(filepath.Dir(handoff), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(handoff, []byte(tt.project), 0600); err != nil {
					t.Fatal(err)
				}
			}
			context := sessionStartContext(t, map[string]any{
				"hook_event_name": "SessionStart", "session_id": "language-test", "source": "startup", "cwd": repo,
			})
			if !strings.Contains(context, tt.want) {
				t.Fatalf("SessionStart context %q does not contain %q", context, tt.want)
			}
			if tt.avoid != "" && strings.Contains(context, tt.avoid) {
				t.Fatalf("SessionStart context %q contains %q", context, tt.avoid)
			}
		})
	}
}

func TestRecordUserLanguageAcceptsEnglishAndIgnoresPlaceholders(t *testing.T) {
	root := t.TempDir()
	RecordUserLanguage(root, "English")
	RecordUserLanguage(root, "<language>")
	if got := lastUserLanguage(root); got != "English" {
		t.Fatalf("lastUserLanguage() = %q, want English", got)
	}
}
