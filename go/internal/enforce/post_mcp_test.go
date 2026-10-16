package enforce

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMCPPostRecoveryAndContext(t *testing.T) {
	root, owner, event := mcpFixture(t, "post-recovery", "codex")
	t.Setenv("PURSER_SESSION_OWNER", owner.String())
	original := runPreDecision
	defer func() { runPreDecision = original }()
	preparations := 0
	runPreDecision = func(map[string]any, string, map[string]any) (map[string]any, error) {
		preparations++
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "updatedInput": map[string]any{"prompt": "effective", "cwd": root}, "additionalContext": "exceptional guidance"}}, nil
	}
	if err := PublishMCPHandoff(root, owner, event); err != nil {
		t.Fatal(err)
	}
	h, err := PrepareMCP(root, owner, "post-recovery", stringValue(event["tool_name"]), mapValue(event["tool_input"]))
	if err != nil {
		t.Fatal(err)
	}
	event["hook_event_name"] = "PostToolUse"
	event["cwd"], event["agent_id"] = "untrusted cwd", "untrusted caller"
	restored, context, err := restoreMCPPostEvent(event, root)
	if err != nil || !reflect.DeepEqual(restored["tool_input"], h.Effective) || restored["cwd"] != root || restored["agent_id"] != nil || context != "exceptional guidance" || preparations != 1 {
		t.Fatalf("recovery: %v %v %q preparations=%d", restored, err, context, preparations)
	}
	if mapValue(event["tool_input"])["prompt"] != "same" {
		t.Fatal("post event mutated")
	}
	for _, name := range []string{"PostToolUse", "PostToolUseFailure"} {
		event["hook_event_name"] = name
		var response map[string]any
		if name == "PostToolUse" {
			response, _ = PostToolUse(event, root)
		} else {
			response = PostToolUseFailure(event, root)
		}
		if got := stringValue(mapValue(response["hookSpecificOutput"])["additionalContext"]); !strings.Contains(got, context) {
			t.Fatalf("%s lost context: %v", name, response)
		}
	}
	if preparations != 1 {
		t.Fatal("post invoked preparation again")
	}
	event["tool_input"] = map[string]any{"prompt": "changed"}
	if _, _, err := restoreMCPPostEvent(event, root); err == nil {
		t.Fatal("changed post input accepted")
	}
	event["tool_input"] = h.Input
	event["session_id"] = "other-session"
	if _, _, err := restoreMCPPostEvent(event, root); err == nil {
		t.Fatal("other session accepted")
	}
}

func TestMCPPostLegacyAndNormalOutputUnchanged(t *testing.T) {
	root, owner, event := mcpFixture(t, "legacy", "codex")
	t.Setenv("PURSER_SESSION_OWNER", owner.String())
	if restored, context, err := restoreMCPPostEvent(event, root); err != nil || context != "" || !reflect.DeepEqual(restored, event) {
		t.Fatalf("legacy recovery: %v", err)
	}
	response := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "updatedToolOutput": []any{map[string]any{"type": "text", "text": "bundle"}}}}
	before := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "updatedToolOutput": []any{map[string]any{"type": "text", "text": "bundle"}}}}
	if got := appendMCPPostContext(response, "PostToolUse", ""); !reflect.DeepEqual(got, before) {
		t.Fatal("normal output grew")
	}
}

func TestMCPPostBypassContext(t *testing.T) {
	root, owner, event := mcpFixture(t, "bypass-post", "codex")
	t.Setenv("PURSER_SESSION_OWNER", owner.String())
	original := runPreDecision
	defer func() { runPreDecision = original }()
	runPreDecision = func(map[string]any, string, map[string]any) (map[string]any, error) {
		return nil, errors.New("injected failure")
	}
	if err := PublishMCPHandoff(root, owner, event); err != nil {
		t.Fatal(err)
	}
	h, _ := PrepareMCP(root, owner, "bypass-post", stringValue(event["tool_name"]), mapValue(event["tool_input"]))
	restored, context, err := restoreMCPPostEvent(event, root)
	if h == nil || err != nil || context == "" || !reflect.DeepEqual(restored["tool_input"], event["tool_input"]) {
		t.Fatalf("bypass recovery: %v %s", err, context)
	}
}
