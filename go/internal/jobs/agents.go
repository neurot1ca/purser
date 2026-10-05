package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var agyModel = regexp.MustCompile(`^gemini-[A-Za-z0-9._-]+$`)

// AgentCommandInput contains the values used to construct an alternate-agent
// CLI invocation. Prefix and Args remain argv elements; no shell is involved.
type AgentCommandInput struct {
	Agent      string
	Prefix     []string
	Tool       string
	CWD        string
	Model      string
	ConfigArgs []string
	Sandbox    *string
	ThreadID   string
	Prompt     string
}

// BuildAgentCommand maps the OpenCode and Antigravity bridge options to argv.
func BuildAgentCommand(in AgentCommandInput) ([]string, error) {
	switch in.Agent {
	case "agy":
		if !agyModel.MatchString(in.Model) {
			return nil, errors.New("Antigravity model must be a Gemini model from agy models")
		}
		if in.Sandbox != nil || len(in.ConfigArgs) != 1 || len(in.ConfigArgs[0]) <= len("model_reasoning_effort=") || in.ConfigArgs[0][:len("model_reasoning_effort=")] != "model_reasoning_effort=" {
			return nil, errors.New("unsupported Antigravity sandbox or config")
		}
		var effort string
		if err := json.Unmarshal([]byte(in.ConfigArgs[0][len("model_reasoning_effort="):]), &effort); err != nil || (effort != "low" && effort != "medium" && effort != "high" && effort != "max") {
			return nil, errors.New("Antigravity effort must be low, medium, high or max")
		}
		command := append([]string{}, in.Prefix...)
		command = append(command, "--print", in.Prompt, "--output-format", "json", "--model", in.Model,
			"--mode", "accept-edits", "--dangerously-skip-permissions", "--effort", effort)
		if in.Tool == "codex-reply" {
			command = append(command, "--conversation", in.ThreadID)
		} else {
			command = append(command, "--new-project", "--add-dir", in.CWD)
		}
		return command, nil
	case "opencode":
		if in.Model == "" || !strings.Contains(in.Model, "/") || in.Model[0] == '/' || in.Model[len(in.Model)-1] == '/' {
			return nil, errors.New("OpenCode model must be provider/model")
		}
		if in.Sandbox != nil {
			return nil, errors.New("OpenCode sandbox has no verified equivalent")
		}
		variants := make([]string, 0, 1)
		for _, arg := range in.ConfigArgs {
			const prefix = "model_reasoning_effort="
			if len(arg) >= len(prefix) && arg[:len(prefix)] == prefix {
				variants = append(variants, arg[len(prefix):])
			}
		}
		if len(variants) > 1 || len(variants) != len(in.ConfigArgs) {
			return nil, errors.New("unsupported OpenCode config")
		}
		command := append([]string{}, in.Prefix...)
		command = append(command, "run", "--format", "json", "--model", in.Model)
		if len(variants) != 0 {
			var variant string
			if err := json.Unmarshal([]byte(variants[0]), &variant); err != nil || variant == "" {
				return nil, errors.New("OpenCode variant must be a nonempty string")
			}
			command = append(command, "--variant", variant)
		}
		if in.Tool == "codex-reply" {
			command = append(command, "--session", in.ThreadID)
		}
		command = append(command, "--", in.Prompt)
		return command, nil
	default:
		return nil, fmt.Errorf("unsupported agent: %s", in.Agent)
	}
}

// ValidateAgentResume rejects cross-agent threads and, for AGY, a resumed
// conversation whose returned identity differs from the requested thread.
func ValidateAgentResume(agent, recordedAgent, requestedThread, resumedIdentity string) error {
	if recordedAgent == "" {
		recordedAgent = "codex"
	}
	if recordedAgent != agent {
		return errors.New("thread belongs to a different agent")
	}
	if agent == "agy" && resumedIdentity != requestedThread {
		return errors.New("Antigravity did not resume the requested conversation; start a fresh call")
	}
	return nil
}
