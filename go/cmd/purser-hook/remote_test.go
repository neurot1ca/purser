package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if len(tools) != 9 {
		t.Fatalf("tools/list has %d tools", len(tools))
	}
	want := map[string]bool{"codex_start": true, "codex_result": true, "codex_cancel": true, "codex_apply": true, "host_diff": true, "host_read": true, "host_validation": true, "host_write": true, "host_release": true}
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

func TestRemoteHostWriteRestrictionsAndBase(t *testing.T) {
	repo := t.TempDir()
	p := filepath.Join(repo, "NEXT.md")
	if err := os.WriteFile(p, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c := &remoteConfig{repo: repo}
	for _, bad := range []string{"other.md", "../NEXT.md", filepath.Join(repo, "NEXT.md")} {
		got := remoteHostWrite(c, map[string]any{"path": bad, "content": "new", "baseSha256": strings.Repeat("0", 64)})
		if got["isError"] != true {
			t.Fatalf("accepted path %q: %#v", bad, got)
		}
	}
	got := remoteHostWrite(c, map[string]any{"path": "NEXT.md", "content": "new", "baseSha256": strings.Repeat("0", 64)})
	if got["isError"] != true || !strings.Contains(got["content"].([]any)[0].(map[string]any)["text"].(string), "NEXT.md changed; read it again") {
		t.Fatalf("stale base accepted: %#v", got)
	}
	h := sha256.Sum256([]byte("old"))
	got = remoteHostWrite(c, map[string]any{"path": "NEXT.md", "content": "new", "baseSha256": hex.EncodeToString(h[:])})
	if got["isError"] == true {
		t.Fatalf("write failed: %#v", got)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "new" {
		t.Fatalf("file=%q err=%v", b, err)
	}
}

func TestRemoteHostReadIncludesSHA256(t *testing.T) {
	repo := t.TempDir()
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	if err := os.WriteFile(filepath.Join(repo, "NEXT.md"), []byte("next"), 0600); err != nil {
		t.Fatal(err)
	}
	got := remoteHostRead(&remoteConfig{repo: repo}, map[string]any{"path": "NEXT.md"})
	h := sha256.Sum256([]byte("next"))
	want := hex.EncodeToString(h[:])
	if got["structuredContent"].(map[string]any)["sha256"] != want || !strings.Contains(got["content"].([]any)[0].(map[string]any)["text"].(string), "sha256: "+want) {
		t.Fatalf("sha missing: %#v", got)
	}
}

func TestRemoteHostValidation(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	id := strings.Repeat("a", 64)
	bad := remoteValidationAt(root, repo, "../bad")
	if bad["isError"] != true {
		t.Fatalf("malformed id accepted: %#v", bad)
	}
	abs, _ := filepath.Abs(repo)
	key := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	dir := filepath.Join(root, "apply_validation", hex.EncodeToString(key[:]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	record := `{"jobId":"` + id + `","status":"passed"}`
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	got := remoteValidationAt(root, repo, id)
	if got["isError"] == true || got["structuredContent"].(map[string]any)["status"] != "passed" {
		t.Fatalf("record not returned: %#v", got)
	}
}

func TestRemoteHostReleaseConfigurationAndOutput(t *testing.T) {
	repo := t.TempDir()
	original := remoteReleaseRunner
	originalEnv, hadEnv := os.LookupEnv("PURSER_REMOTE_RELEASE")
	t.Cleanup(func() {
		remoteReleaseRunner = original
		if hadEnv {
			_ = os.Setenv("PURSER_REMOTE_RELEASE", originalEnv)
		} else {
			_ = os.Unsetenv("PURSER_REMOTE_RELEASE")
		}
	})
	remoteReleaseRunner = func(dir, name string, args []string, timeout time.Duration) (string, int, error) {
		switch name {
		case "git":
			if len(args) > 0 && args[0] == "branch" {
				return "main", 0, nil
			}
			return "", 0, nil
		}
		return "", 0, nil
	}
	_ = os.Unsetenv("PURSER_REMOTE_RELEASE")
	got := remoteHostRelease(&remoteConfig{repo: repo}, nil)
	if got["isError"] != true || !strings.Contains(got["content"].([]any)[0].(map[string]any)["text"].(string), "no release command configured on the host (PURSER_REMOTE_RELEASE)") {
		t.Fatalf("unset release command accepted: %#v", got)
	}
	missing := filepath.Join(repo, "missing.sh")
	_ = os.Setenv("PURSER_REMOTE_RELEASE", missing)
	got = remoteHostRelease(&remoteConfig{repo: repo}, nil)
	if got["isError"] != true || !strings.Contains(got["content"].([]any)[0].(map[string]any)["text"].(string), "no release command configured on the host (PURSER_REMOTE_RELEASE)") {
		t.Fatalf("missing release command accepted: %#v", got)
	}
	script := filepath.Join(repo, "release.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Setenv("PURSER_REMOTE_RELEASE", script)
	remoteReleaseRunner = func(dir, name string, args []string, timeout time.Duration) (string, int, error) {
		if name == "git" {
			if len(args) > 0 && args[0] == "branch" {
				return "main", 0, nil
			}
			return "", 0, nil
		}
		return "failed release output", 7, fmt.Errorf("release failed")
	}
	got = remoteHostRelease(&remoteConfig{repo: repo}, nil)
	structured := got["structuredContent"].(map[string]any)
	if got["isError"] != true || structured["exitCode"] != 7 || !strings.Contains(structured["output"].(string), "failed release output") {
		t.Fatalf("failed release not reported: %#v", got)
	}
	remoteReleaseRunner = func(dir, name string, args []string, timeout time.Duration) (string, int, error) {
		if name == "git" {
			if len(args) > 0 && args[0] == "branch" {
				return "main", 0, nil
			}
			return "", 0, nil
		}
		return "passed release output", 0, nil
	}
	got = remoteHostRelease(&remoteConfig{repo: repo}, nil)
	structured = got["structuredContent"].(map[string]any)
	if got["isError"] == true || structured["exitCode"] != 0 || !strings.Contains(structured["output"].(string), "passed release output") {
		t.Fatalf("passing release failed: %#v", got)
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
