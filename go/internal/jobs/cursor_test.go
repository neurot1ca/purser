package jobs

import (
	"reflect"
	"testing"
)

func TestCursorAgentCommandUsesStdinAndResumes(t *testing.T) {
	got, err := BuildAgentCommand(AgentCommandInput{Agent: "cursor", Prefix: []string{"cursor-agent.cmd"}, Tool: "codex-reply", Model: "claude-4.5-sonnet", ThreadID: "session-1", Prompt: "not argv"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cursor-agent.cmd", "--print", "--output-format", "stream-json", "--model", "claude-4.5-sonnet", "--force", "--resume", "session-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Cursor argv = %#v, want %#v", got, want)
	}

	got, err = BuildAgentCommand(AgentCommandInput{Agent: "cursor", Prefix: []string{"cursor-agent.cmd"}, Tool: "codex", Model: "claude-4.5-sonnet", Sandbox: stringPointer("read-only")})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"cursor-agent.cmd", "--print", "--output-format", "stream-json", "--model", "claude-4.5-sonnet", "--mode", "ask", "--trust"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sandboxed Cursor argv = %#v, want %#v", got, want)
	}
}

func TestCursorRejectsUnsupportedOptions(t *testing.T) {
	for _, in := range []AgentCommandInput{
		{Agent: "cursor", Prefix: []string{"cursor-agent"}, Tool: "codex", Model: "auto"},
		{Agent: "cursor", Prefix: []string{"cursor-agent"}, Tool: "codex", Model: "claude-4.5-sonnet", Sandbox: stringPointer("workspace-write")},
		{Agent: "cursor", Prefix: []string{"cursor-agent"}, Tool: "codex", Model: "claude-4.5-sonnet", ConfigArgs: []string{`model_reasoning_effort="high"`}},
	} {
		if _, err := BuildAgentCommand(in); err == nil {
			t.Fatalf("Cursor accepted unsupported launch: %#v", in)
		}
	}
}

func TestCursorEventRequiresCompleteMatchingSession(t *testing.T) {
	stream := NewEventStream("cursor", "session-1")
	stream.Write([]byte(`{"type":"system","subtype":"init","session_id":"session-1"}` + "\n"))
	stream.Write([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"session-2"}`))
	got := stream.Result(false, 0, "")
	if got.Status == "success" || got.Final != "" {
		t.Fatalf("mismatched Cursor resume passed: %#v", got)
	}
}

func TestCursorEventAcceptsOnlySuccessfulTerminalOutput(t *testing.T) {
	stream := NewEventStream("cursor")
	stream.Write([]byte(`{"type":"system","subtype":"init","session_id":"session-1"}` + "\n"))
	stream.Write([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"session-1"}`))
	got := stream.Result(false, 0, "")
	if got.Status != "success" || got.ThreadID != "session-1" || got.Final != "done" {
		t.Fatalf("valid Cursor terminal rejected: %#v", got)
	}
	stream = NewEventStream("cursor")
	stream.Write([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"session-1"}`))
	got = stream.Result(false, 1, "process failed")
	if got.Status == "success" || got.Final != "" {
		t.Fatalf("Cursor terminal with failed process passed: %#v", got)
	}

	stream = NewEventStream("cursor")
	stream.Write([]byte(`{"type":"system","subtype":"init","session_id":"session-1"}`))
	got = stream.Result(false, 0, "")
	if got.Status == "success" || got.Final != "" {
		t.Fatalf("Cursor stream without terminal result passed: %#v", got)
	}
}

func TestCursorStartRejectsUnsupportedModelBeforeLaunch(t *testing.T) {
	args := map[string]any{"call": "00000000-0000-0000-0000-000000000001", "prompt": "work", "cwd": t.TempDir(), "model": "auto"}
	_, failure := ValidateStart(args, StartValidationOptions{Agent: "cursor", FixedAgent: true})
	if failure == nil {
		t.Fatal("Cursor start accepted an automatic model")
	}
}

func TestCursorNeverProvesSafeRetryFromSilence(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		code   *int
	}{{}, {stderr: "429 rate limited"}, {code: intPointer(1)}} {
		evidence := LaunchFailureEvidence("cursor", nil, tc.stderr, true, false, false, tc.code)
		if evidence["noWorkProof"] == true {
			t.Fatalf("Cursor failure proved replay safe: %#v", evidence)
		}
	}
}

func intPointer(value int) *int { return &value }
