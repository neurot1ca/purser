package enforce

import (
	"strings"
	"testing"
)

func TestPostToolUseFailureDispatchesEveryCodexToolKind(t *testing.T) {
	cases := []struct {
		tool string
		want string
	}{
		{"mcp__codex-main__codex", "PostToolUseFailure"},
		{"mcp__codex-main__codex-reply", "PostToolUseFailure"},
		{"mcp__codex-main__codex_start", "PostToolUseFailure"},
		{"mcp__codex-main__codex_result", "PostToolUse"},
		{"mcp__codex-main__codex_cancel", "PostToolUse"},
	}
	for _, item := range cases {
		t.Run(strings.TrimPrefix(item.tool, "mcp__codex-main__"), func(t *testing.T) {
			response := PostToolUseFailure(map[string]any{"hook_event_name": "PostToolUseFailure", "tool_name": item.tool,
				"session_id": "dispatch-session", "tool_use_id": "dispatch-tool"}, t.TempDir())
			if response == nil {
				t.Fatal("dispatch returned no response")
			}
			output := mapValue(response["hookSpecificOutput"])
			if got := stringValue(output["hookEventName"]); got != item.want {
				t.Fatalf("hookEventName = %q, want %q; response=%#v", got, item.want, response)
			}
		})
	}
}
