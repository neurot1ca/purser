package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func remoteRequest(t *testing.T, h http.Handler, token, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v
}
func TestRemoteAuthAndAllowedTools(t *testing.T) {
	repo := t.TempDir()
	h := remoteHTTPHandler(strings.Repeat("t", 48), &remoteConfig{repo: repo, session: "remote-test"})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, token := range []string{"", "wrong"} {
		if code, _ := remoteRequest(t, h, token, body); code != 401 {
			t.Fatalf("unauthorized status=%d", code)
		}
	}
	code, v := remoteRequest(t, h, strings.Repeat("t", 48), body)
	if code != 200 {
		t.Fatalf("tools/list status=%d", code)
	}
	tools := v["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("tools/list has %d tools", len(tools))
	}
	want := map[string]bool{"codex_start": true, "codex_result": true, "codex_cancel": true, "codex_apply": true, "host_diff": true, "host_read": true}
	for _, item := range tools {
		name := item.(map[string]any)["name"].(string)
		if !want[name] {
			t.Fatalf("unexpected tool %q", name)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing tools: %v", want)
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"codex","arguments":{}}}`
	code, v = remoteRequest(t, h, strings.Repeat("t", 48), call)
	if code != 200 || v["error"] == nil {
		t.Fatalf("unexpected tool accepted: %v", v)
	}
}

func TestRemoteNotificationReturnsAcceptedWithoutBody(t *testing.T) {
	h := remoteHTTPHandler(strings.Repeat("t", 48), &remoteConfig{repo: t.TempDir(), session: "remote-test"})
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 48))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification response status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestRemoteAcceptOnlyApplyDoesNotRequireCommitMessage(t *testing.T) {
	result := map[string]any{"structuredContent": map[string]any{"accepted": true}}
	if failure := remoteCommitApplied(t.TempDir(), map[string]any{"jobId": "job", "acceptOnly": true}, result); failure != "" {
		t.Fatalf("acceptOnly apply unexpectedly required commitMessage: %s", failure)
	}
}

func TestRemoteHostReadAndStartRestrictions(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	h := remoteHTTPHandler(strings.Repeat("t", 48), &remoteConfig{repo: repo, session: "remote-test"})
	token := strings.Repeat("t", 48)
	for _, path := range []string{"../secret", filepath.Join(t.TempDir(), "secret"), "..\\secret"} {
		arg, _ := json.Marshal(map[string]any{"path": path})
		body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"host_read","arguments":` + string(arg) + `}}`
		_, v := remoteRequest(t, h, token, body)
		if v["result"] == nil || v["result"].(map[string]any)["isError"] != true {
			t.Fatalf("read outside allowlist %q: %v", path, v)
		}
	}
	body := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"codex_start","arguments":{"prompt":"no job","cwd":"/elsewhere"}}}`
	_, v := remoteRequest(t, h, token, body)
	if v["result"] == nil || v["result"].(map[string]any)["isError"] != true {
		t.Fatalf("different cwd accepted: %v", v)
	}
	if _, e := os.Stat(filepath.Join(repo, "NEXT.md")); !os.IsNotExist(e) {
		t.Fatal("unexpected test file")
	}
}

func TestRemoteUpdatedOutputReturnsReplacementBundle(t *testing.T) {
	bundle := []any{map[string]any{"type": "text", "text": `{"changed_paths_sha256":"abc"}`}}
	post := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "updatedMCPToolOutput": bundle, "additionalContext": "note"}}
	if got := remoteUpdatedOutput(post); len(got) != 1 || got[0].(map[string]any)["text"] != bundle[0].(map[string]any)["text"] {
		t.Fatalf("replacement bundle dropped: %#v", got)
	}
	if got := remoteUpdatedOutput(map[string]any{}); got != nil {
		t.Fatalf("unexpected replacement: %#v", got)
	}
}
