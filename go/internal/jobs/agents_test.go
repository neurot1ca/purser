package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type agentFixture struct {
	Oracle string      `json:"oracle"`
	Cases  []agentCase `json:"cases"`
}

type agentCase struct {
	Agent string   `json:"agent"`
	Kind  string   `json:"kind"`
	Argv  []string `json:"argv"`
	Error string   `json:"error"`
}

func TestAgentCommandPythonParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_agents", "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture agentFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "codex_exec_mcp._agent_command and Server._run_tool_once resume identity" {
		t.Fatalf("unexpected oracle: %q", fixture.Oracle)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Agent+"/"+test.Kind, func(t *testing.T) {
			if test.Kind == "resume_identity_mismatch" {
				otherAgent := "opencode"
				if test.Agent == "opencode" {
					otherAgent = "agy"
				}
				err := ValidateAgentResume(test.Agent, otherAgent, "thread-123", "")
				assertAgentError(t, err, test.Error)
				return
			}
			input := AgentCommandInput{
				Agent: test.Agent, Prefix: []string{"agent-bin", "--prefix"},
				Tool: "codex", CWD: "C:/repo", Model: "google/gemini-test",
				ConfigArgs: []string{`model_reasoning_effort="high"`}, Prompt: "prompt text",
			}
			if test.Agent == "agy" {
				input.Model = "gemini-test"
			}
			if test.Kind == "codex-reply" {
				if err := ValidateAgentResume(test.Agent, test.Agent, "thread-123", "thread-123"); err != nil {
					t.Fatal(err)
				}
				input.Tool, input.ThreadID = "codex-reply", "thread-123"
			}
			if test.Kind == "unsupported_sandbox" {
				input.Sandbox = stringPointer("read-only")
			}
			if test.Kind == "unsupported_config" {
				input.ConfigArgs = []string{"unsupported_option=true"}
			}
			got, err := BuildAgentCommand(input)
			if test.Error != "" {
				assertAgentError(t, err, test.Error)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.Argv) {
				t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", got, test.Argv)
			}
		})
	}
}

func TestClaudeWorkerCarriesEffortAndSuppressesNestedHooks(t *testing.T) {
	command, err := BuildAgentCommand(AgentCommandInput{Agent: "claude", Prefix: []string{"claude"}, Tool: "codex", CWD: t.TempDir(),
		Model: "claude-sonnet-4-5", ConfigArgs: []string{`model_reasoning_effort="high"`}, Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--setting-sources", "", "--restricted", "--permission-mode", "bypassPermissions", "--print", "--output-format", "json", "--model", "claude-sonnet-4-5", "--effort", "high", "--tools", "Read,Edit,Write,Glob,Grep,Bash", "work"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("Claude command argv mismatch\n got: %#v\nwant: %#v", command, want)
	}
	withoutEffort, err := BuildAgentCommand(AgentCommandInput{Agent: "claude", Prefix: []string{"claude"}, Tool: "codex", Model: "claude-sonnet-4-5", Prompt: "work"})
	if err != nil {
		t.Fatalf("Claude launch rejected omitted effort: %v", err)
	}
	for _, arg := range withoutEffort {
		if arg == "--effort" {
			t.Fatalf("omitted Claude effort emitted --effort: %#v", withoutEffort)
		}
	}
	if _, err := BuildAgentCommand(AgentCommandInput{Agent: "claude", Prefix: []string{"claude"}, Tool: "codex", Model: "unsupported", ConfigArgs: []string{`model_reasoning_effort="high"`}, Prompt: "work"}); err == nil {
		t.Fatal("Claude launch accepted an unsupported model")
	}
}

func TestClaudePlanIsReadOnlyAndAliasNoneOmitsEffort(t *testing.T) {
	command, err := BuildAgentCommand(AgentCommandInput{Agent: "claude", Prefix: []string{"claude"}, Tool: "codex", Model: "opus",
		Sandbox: stringPointer("read-only"), Prompt: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "--setting-sources", "", "--restricted", "--print", "--output-format", "json", "--model", "opus", "--tools", "Read,Glob,Grep", "plan"}
	if !reflect.DeepEqual(command, want) {
		t.Fatalf("Claude plan argv mismatch\n got: %#v\nwant: %#v", command, want)
	}
	command, err = BuildAgentCommand(AgentCommandInput{Agent: "claude", Prefix: []string{"claude"}, Tool: "codex", Model: "haiku",
		ConfigArgs: []string{`model_reasoning_effort="none"`}, Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range command {
		if arg == "--effort" {
			t.Fatalf("Claude none effort must omit --effort: %#v", command)
		}
	}
}

func TestValidateStartAcceptsClaudePlanJob(t *testing.T) {
	t.Setenv("PURSER_AGENT", "")
	args := map[string]any{"call": "00000000-0000-0000-0000-000000000001", "prompt": "plan", "cwd": t.TempDir(), "model": "opus", "sandbox": "read-only"}
	request, failure := ValidateStart(args, StartValidationOptions{Agent: "claude", FixedAgent: true})
	if failure != nil || request.Agent != "claude" {
		t.Fatalf("Claude plan start failed validation: request=%#v failure=%#v", request, failure)
	}
}

func assertAgentError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func stringPointer(value string) *string { return &value }
