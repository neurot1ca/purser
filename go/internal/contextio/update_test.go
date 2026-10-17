package contextio

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupPluginUpdateTest(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", filepath.Join(root, "state"))
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(filepath.Join(plugin, ".claude-plugin"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]string{"name": "purser", "version": version})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, ".claude-plugin", "plugin.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	t.Setenv("PURSER_NO_UPDATE_CHECK", "")
	return plugin
}

func TestPurserUpdateNoticeAnnouncedOnce(t *testing.T) {
	setupPluginUpdateTest(t, "1.2.3")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("User-Agent") != "purser" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request: %s %v", r.Method, r.Header)
		}
		_, _ = w.Write([]byte(`{"tag_name":"v1.3.0"}`))
	}))
	defer server.Close()
	CheckPurserUpdateAt(server.URL)
	want := "⚓ Purser 1.3.0 is available (you have 1.2.3). Update with /plugin marketplace update purser-claude-orchestrator, then restart Claude Code."
	if got := TakePurserUpdateNotice(); got != want {
		t.Fatalf("first notice = %q, want %q", got, want)
	}
	if got := TakePurserUpdateNotice(); got != "" {
		t.Fatalf("repeated notice = %q", got)
	}
}

func TestSessionStartPutsPurserUpdateInSystemMessage(t *testing.T) {
	setupPluginUpdateTest(t, "1.2.3")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.3.0"}`))
	}))
	defer server.Close()
	CheckPurserUpdateAt(server.URL)
	var response map[string]any
	if err := json.Unmarshal(SessionStart(map[string]any{"session_id": "session"}), &response); err != nil {
		t.Fatal(err)
	}
	want := "⚓ Purser 1.3.0 is available (you have 1.2.3). Update with /plugin marketplace update purser-claude-orchestrator, then restart Claude Code."
	if response["systemMessage"] != want {
		t.Fatalf("systemMessage = %v, want %q", response["systemMessage"], want)
	}
	hookOutput, ok := response["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("hookSpecificOutput = %T, want object", response["hookSpecificOutput"])
	}
	if strings.Contains(hookOutput["additionalContext"].(string), "Purser 1.3.0 is available") {
		t.Fatal("release notice was added to additionalContext")
	}
	if got := TakePurserUpdateNotice(); got != "" {
		t.Fatalf("SessionStart did not mark notice delivered: %q", got)
	}
}

func TestPurserUpdateNoticeSameOrOlderIsSilent(t *testing.T) {
	for _, latest := range []string{"v1.2.3", "v1.2.2"} {
		t.Run(latest, func(t *testing.T) {
			setupPluginUpdateTest(t, "1.2.3")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"tag_name":"` + latest + `"}`))
			}))
			defer server.Close()
			CheckPurserUpdateAt(server.URL)
			if got := TakePurserUpdateNotice(); got != "" {
				t.Fatalf("notice = %q", got)
			}
		})
	}
}

func TestPurserUpdateNetworkErrorIsSilent(t *testing.T) {
	setupPluginUpdateTest(t, "1.2.3")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	CheckPurserUpdateAt(server.URL)
	CheckPurserUpdateAt(server.URL)
	if requests != 1 {
		t.Fatalf("requests today = %d, want 1", requests)
	}
	if got := TakePurserUpdateNotice(); got != "" {
		t.Fatalf("notice = %q", got)
	}
}

func TestPurserUpdateOptOutMakesNoRequest(t *testing.T) {
	setupPluginUpdateTest(t, "1.2.3")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"tag_name":"v1.3.0"}`))
	}))
	defer server.Close()
	t.Setenv("PURSER_NO_UPDATE_CHECK", "1")
	CheckPurserUpdateAt(server.URL)
	if requests != 0 {
		t.Fatalf("request count = %d, want 0", requests)
	}
}
