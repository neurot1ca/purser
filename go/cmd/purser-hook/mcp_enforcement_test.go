package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/enforce"
	"purser-hook/internal/waiter"
)

func TestMCPPreparationBindingAndPolicyParity(t *testing.T) {
	parent := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, "mcp-preparation-")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.Abs(root)
	defer os.RemoveAll(root)
	owner, err := waiter.PinOwner(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "stale-env-session")
	e := map[string]any{"hook_event_name": "PreToolUse", "session_id": "current-hook-session", "tool_use_id": "exact-join",
		"tool_name": "mcp__plugin_purser_codex-main__codex_apply", "cwd": root, "agent_id": "agent",
		"tool_input": map[string]any{"jobId": "job", "acceptOnly": true, "sessionId": "forged"}}
	if err := enforce.PublishMCPHandoff(root, owner, e); err != nil {
		t.Fatal(err)
	}
	for _, wire := range []string{
		`{"name":"codex_apply","arguments":{},"_meta":{}}`,
		`{"name":"codex_apply","arguments":{},"_meta":{"claudecode/toolUseId":"exact-join","claudecode/toolUseId":"other"}}`,
		`{"name":"codex_apply","arguments":{},"_meta":{"claudecode/toolUseId":"exact-join"}}`,
	} {
		if _, err := prepareMCPCall(root, owner, []byte(wire)); err == nil {
			t.Fatal("bad metadata/binding accepted")
		}
	}
	params, _ := json.Marshal(map[string]any{"name": "codex_apply", "arguments": e["tool_input"], "_meta": map[string]any{"claudecode/toolUseId": "exact-join"}})
	want, _ := enforce.Pre(e, root)
	h, err := prepareMCPCall(root, owner, params)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.Response, want) || h.SessionID != "current-hook-session" || !reflect.DeepEqual(h.Effective, e["tool_input"]) {
		t.Fatalf("policy/identity mismatch: %+v", h)
	}
	read, err := enforce.ReadPreparedMCPHandoff(root, owner, h.SessionID, h.ToolUseID)
	if err != nil || !reflect.DeepEqual(read.Response, want) {
		t.Fatalf("post recovery: %v", err)
	}
	if _, err := enforce.ReadPreparedMCPHandoff(root, owner, "forged", h.ToolUseID); err == nil {
		t.Fatal("wrong session recovered")
	}
	if _, err := prepareMCPCall(root, owner, params); err == nil {
		t.Fatal("replayed preparation")
	}
}
