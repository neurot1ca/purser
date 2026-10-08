package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const startDefaultTimeout = 86400

type StartRequest struct {
	JobID          string         `json:"jobId"`
	Call           string         `json:"call"`
	Prompt         string         `json:"prompt"`
	Agent          string         `json:"agent"`
	Mode           string         `json:"mode"`
	Name           string         `json:"name"`
	TimeoutSeconds float64        `json:"timeoutSeconds"`
	Args           map[string]any `json:"args"`
	ExistingResult map[string]any `json:"existingResult,omitempty"`
}

type StartValidationOptions struct {
	Agent           string
	FixedAgent      bool
	ConfiguredAgent func() (string, error)
	ValidateAgent   func(string) error
	LookupExisting  func(string) map[string]any
}

// ValidateStart checks duplicates after UUID validation, before timeout and launch validation.
func ValidateStart(args map[string]any, opt StartValidationOptions) (StartRequest, map[string]any) {
	fail := func(code, message string) (StartRequest, map[string]any) {
		return StartRequest{}, jobResult(map[string]any{"state": "FAILED", "errorCode": code, "message": message})
	}
	agent := opt.Agent
	if value, exists := args["agent"]; exists {
		var ok bool
		agent, ok = value.(string)
		if !ok {
			return fail("INVALID_ARGUMENT", "Invalid agent")
		}
	}
	if agent == "" && opt.Agent == "" {
		agent = "codex"
	}
	if !validStartAgent(agent) {
		return fail("INVALID_ARGUMENT", "Invalid agent")
	}
	if _, supplied := args["agent"]; !supplied && !opt.FixedAgent && os.Getenv("PURSER_AGENT") == "" && opt.ConfiguredAgent != nil {
		var err error
		agent, err = opt.ConfiguredAgent()
		if err != nil {
			return fail("INVALID_CONFIG", err.Error())
		}
		if !validStartAgent(agent) {
			return fail("INVALID_ARGUMENT", "Invalid agent")
		}
	}
	call, callOK := args["call"].(string)
	prompt, promptOK := args["prompt"].(string)
	if !callOK || call == "" || !promptOK {
		return fail("INVALID_ARGUMENT", "call and prompt are required")
	}
	if !validStartUUID(call) {
		return fail("INVALID_ARGUMENT", "call must be a UUID")
	}
	id := sha256.Sum256([]byte(call))
	jobID := hex.EncodeToString(id[:])
	if opt.LookupExisting != nil {
		if existing := opt.LookupExisting(jobID); existing != nil {
			return StartRequest{JobID: jobID, Call: call, Prompt: prompt, Args: args, ExistingResult: existing}, nil
		}
	}
	timeout := any(startDefaultTimeout)
	if v, ok := args["timeoutSeconds"]; ok {
		timeout = v
	}
	seconds, ok := startTimeout(timeout)
	if !ok {
		return fail("INVALID_ARGUMENT", "timeoutSeconds must be positive")
	}
	mode, exists := args["mode"].(string)
	if !exists {
		if _, supplied := args["mode"]; supplied {
			return fail("INVALID_ARGUMENT", "invalid mode")
		}
		mode = "new"
	}
	if mode != "new" && mode != "reply" {
		return fail("INVALID_ARGUMENT", "invalid mode")
	}
	if mode == "new" {
		cwd, exists := args["cwd"]
		if !exists || cwd == "" {
			if _, err := resolveRepositoryRoot(); err != nil {
				return fail("INVALID_ARGUMENT", "Repository root could not be resolved for Codex launch")
			}
		}
	}
	name := "codex"
	if mode == "reply" {
		name = "codex-reply"
	}
	if err := validateStartLaunch(args, agent, mode, name, opt); err != nil {
		return fail("INVALID_ARGUMENT", "Invalid Codex launch arguments: "+err.Error())
	}
	return StartRequest{JobID: jobID, Call: call, Prompt: prompt, Agent: agent, Mode: mode, Name: name, TimeoutSeconds: seconds, Args: args}, nil
}

func resolveRepositoryRoot() (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	command := exec.Command("git", "-C", workingDir, "rev-parse", "--show-toplevel")
	root, err := command.Output()
	if err != nil || strings.TrimSpace(string(root)) == "" {
		return "", strconv.ErrSyntax
	}
	return strings.TrimSpace(string(root)), nil
}

func jobResult(value map[string]any) map[string]any {
	keys := []string{"state", "errorCode", "message"}
	if _, failed := value["errorCode"]; !failed {
		keys = []string{"jobId", "state", "acceptedAt", "deadlineAt"}
	} else if _, hasID := value["jobId"]; hasID {
		keys = []string{"jobId", "state", "errorCode", "message"}
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if v, ok := value[key]; ok {
			parts = append(parts, pythonQuote(key)+": "+pythonQuote(v))
		}
	}
	text := "{" + strings.Join(parts, ", ") + "}"
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "structuredContent": value, "isError": value["errorCode"] != nil}
}

func pythonQuote(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return strings.NewReplacer(`\u2028`, " ", `\u2029`, " ").Replace(strings.TrimSuffix(b.String(), "\n"))
}

func validStartAgent(s string) bool {
	return s == "codex" || s == "opencode" || s == "agy" || s == "claude"
}
func validStartUUID(s string) bool {
	s = strings.ReplaceAll(s, "urn:", "")
	s = strings.ReplaceAll(s, "uuid:", "")
	s = strings.Trim(s, "{}")
	s = strings.ReplaceAll(s, "-", "")
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16
}
func startTimeout(v any) (float64, bool) {
	var n float64
	switch x := v.(type) {
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case float64:
		n = x
	case json.Number:
		n, _ = x.Float64()
	default:
		return 0, false
	}
	return n, n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func validateStartLaunch(a map[string]any, agent, mode, name string, opt StartValidationOptions) error {
	if opt.ValidateAgent != nil {
		if err := opt.ValidateAgent(agent); err != nil {
			return err
		}
	}
	if mode != "new" {
		return nil
	}
	cwdText, _ := os.Getwd()
	if cwd, present := a["cwd"]; present {
		text, ok := cwd.(string)
		if !ok {
			return strconv.ErrSyntax
		}
		if text != "" {
			cwdText = text
		}
	}
	if cwdText == "" {
		return strconv.ErrSyntax
	}
	if v, ok := a["model"]; ok && v != nil {
		if _, ok = v.(string); !ok {
			return strconv.ErrSyntax
		}
	}
	cfg, ok := a["config"]
	if !ok {
		cfg = map[string]any{}
	}
	if _, ok = cfg.(map[string]any); !ok {
		if _, ok = cfg.(OrderedObject); !ok {
			return strconv.ErrSyntax
		}
	}
	configArgs, err := FlattenConfig(cfg)
	if err != nil {
		return err
	}
	if v := a["sandbox"]; v != nil && v != "read-only" && v != "workspace-write" && v != "danger-full-access" {
		return strconv.ErrSyntax
	}
	if v, ok := a["worktree"]; ok {
		if _, ok = v.(bool); !ok {
			return strconv.ErrSyntax
		}
	}
	if v := a["approval-policy"]; v != nil && v != "on-request" && v != "never" {
		return strconv.ErrSyntax
	}
	for _, k := range []string{"developer-instructions", "base-instructions", "compact-prompt"} {
		if v, ok := a[k]; ok {
			if _, ok = v.(string); !ok {
				return strconv.ErrSyntax
			}
		}
	}
	if agent == "opencode" || agent == "agy" {
		if a["approval-policy"] != nil {
			return strconv.ErrSyntax
		}
		for _, k := range []string{"developer-instructions", "base-instructions", "compact-prompt"} {
			if _, ok := a[k]; ok {
				return strconv.ErrSyntax
			}
		}
		_, err = BuildAgentCommand(AgentCommandInput{Agent: agent, Tool: name, CWD: cwdText, Model: stringArg(a["model"]), ConfigArgs: configArgs, Prompt: stringArg(a["prompt"])})
		return err
	}
	if agent == "claude" {
		var sandbox *string
		if value, ok := a["sandbox"].(string); ok {
			sandbox = &value
		}
		_, err = BuildAgentCommand(AgentCommandInput{Agent: agent, Tool: name, CWD: cwdText, Model: stringArg(a["model"]), ConfigArgs: configArgs, Sandbox: sandbox, Prompt: stringArg(a["prompt"])})
		return err
	}
	return nil
}
func stringArg(v any) string { s, _ := v.(string); return s }
