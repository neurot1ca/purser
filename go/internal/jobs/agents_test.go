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

func assertAgentError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func stringPointer(value string) *string { return &value }
