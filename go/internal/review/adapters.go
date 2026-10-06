package review

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var reviewerCommands = map[string]string{"agy": "agy", "opencode": "opencode", "codex": "codex"}
var openCodePermissions = map[string]string{"edit": "ask", "write": "ask", "bash": "ask", "webfetch": "ask", "task": "ask", "external_directory": "ask"}

const (
	fastProcessFailureRetrySeconds = 60.0
	minRetryRemainingSeconds       = 5.0
	openCodeConfigEnv              = "OPENCODE_CONFIG_CONTENT"
)

// Adapter describes reviewer command construction, confinement, and response parsing.
type Adapter interface {
	Provider() string
	AllowedModel() string
	Family(model string) string
	ValidateModel(model any) error
	BuildCommand(prompt string, schema map[string]any, config map[string]any, options CommandOptions) ([]string, error)
	Environment(command []string) map[string]string
	LaunchOptions() ProcessOptions
	ConfinementConfig(command []string) (string, []byte)
	ParseEnvelope(stdout string) (map[string]any, error)
}

type CommandOptions struct {
	WorkingDirectory, RepositoryRoot string
	AddDirs, ExtraFlags              []string
}
type ProcessOptions struct {
	CreationFlags          uint32
	HideWindow, NewConsole bool
}
type CommandRefused struct{ Message string }

func (e *CommandRefused) Error() string { return e.Message }

type reviewerAdapter struct{ provider string }

var reviewerAdapters = map[string]Adapter{"agy": reviewerAdapter{"agy"}, "opencode": reviewerAdapter{"opencode"}, "codex": reviewerAdapter{"codex"}}

func ReviewerAdapter(provider string) (Adapter, error) {
	a, ok := reviewerAdapters[provider]
	if !ok {
		return nil, fmt.Errorf("unknown reviewer adapter %q", provider)
	}
	return a, nil
}
func RegisterReviewerAdapter(provider string, adapter Adapter) error {
	if provider == "" || adapter == nil || adapter.Provider() == "" {
		return errors.New("reviewer adapter registration is invalid")
	}
	reviewerAdapters[provider] = adapter
	return nil
}
func (a reviewerAdapter) Provider() string { return a.provider }
func (a reviewerAdapter) AllowedModel() string {
	if a.provider == "agy" {
		return pinnedModel
	}
	return ""
}
func (a reviewerAdapter) Family(model string) string {
	switch a.provider {
	case "agy":
		return "google"
	case "codex":
		return "openai"
	case "opencode":
		p, _, ok := strings.Cut(model, "/")
		if ok {
			return vendorFamily(p)
		}
	}
	return ""
}
func (a reviewerAdapter) ValidateModel(model any) error {
	switch a.provider {
	case "agy":
		if model != pinnedModel {
			return fmt.Errorf("reviewer1_model_requested must equal the pinned id %s", pinnedModel)
		}
	case "opencode":
		value, ok := model.(string)
		parts := strings.Split(value, "/")
		if !ok || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return errors.New("OpenCode reviewer model must be provider/model")
		}
	case "codex":
		if model != nil {
			value, ok := model.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return errors.New("Codex reviewer model must be a nonempty string")
			}
		}
	}
	return nil
}
func (a reviewerAdapter) BuildCommand(prompt string, schema map[string]any, config map[string]any, options CommandOptions) ([]string, error) {
	executable, err := reviewerExecutable(config, a.provider)
	if err != nil {
		return nil, err
	}
	model := config["reviewer1_model_requested"]
	if err := a.ValidateModel(model); err != nil {
		return nil, err
	}
	switch a.provider {
	case "agy":
		encoded, err := canonicalJSON(schema)
		if err != nil {
			return nil, err
		}
		command := []string{executable, "--print", prompt, "--output-format", "json", "--json-schema", string(encoded), "--model", model.(string), "--mode", "plan", "--sandbox"}
		for _, dir := range options.AddDirs {
			resolved, err := resolveReviewerPath(dir)
			if err != nil {
				return nil, err
			}
			command = append(command, "--add-dir", resolved)
		}
		command = append(command, options.ExtraFlags...)
		if err := RefuseUnsafeFlags(command, options.RepositoryRoot); err != nil {
			return nil, err
		}
		return command, nil
	case "opencode":
		agent, ok := config["reviewer1_agent"]
		if !ok {
			agent = "reviewer"
		}
		name, ok := agent.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, errors.New("reviewer1_agent must be a nonempty name")
		}
		cwd, err := resolveReviewerPath(options.WorkingDirectory)
		if err != nil {
			return nil, err
		}
		return []string{executable, "run", "--format", "json", "--model", model.(string), "--agent", name, "--dir", cwd, prompt}, nil
	case "codex":
		cwd, err := resolveReviewerPath(options.WorkingDirectory)
		if err != nil {
			return nil, err
		}
		command := []string{executable, "exec", "--json", "--skip-git-repo-check", "-s", "read-only", "-c", `approval_policy="never"`, "-C", cwd}
		if model != nil && model != "" {
			command = append(command, "-m", model.(string))
		}
		return append(command, prompt), nil
	}
	return nil, fmt.Errorf("unknown reviewer adapter %q", a.provider)
}
func (a reviewerAdapter) Environment(command []string) map[string]string {
	if a.provider != "opencode" {
		return map[string]string{}
	}
	name := flagValue(command, "--agent")
	permissions := map[string]any{"permission": openCodePermissions, "agent": map[string]any{name: map[string]any{"permission": openCodePermissions}}}
	encoded, _ := canonicalJSON(permissions)
	return map[string]string{openCodeConfigEnv: string(encoded)}
}
func (a reviewerAdapter) LaunchOptions() ProcessOptions {
	if runtime.GOOS != "windows" {
		return ProcessOptions{}
	}
	if a.provider == "agy" {
		return ProcessOptions{CreationFlags: 0x10, HideWindow: true, NewConsole: true}
	}
	return ProcessOptions{CreationFlags: 0x08000000, HideWindow: true}
}

// Python adapters pass OpenCode confinement as OPENCODE_CONFIG_CONTENT; none writes a separate file.
func (reviewerAdapter) ConfinementConfig([]string) (string, []byte) { return "", nil }
func (a reviewerAdapter) ParseEnvelope(stdout string) (map[string]any, error) {
	parsed, err := ParseEnvelope(a.provider, []byte(stdout), true)
	if err != nil {
		return nil, err
	}
	return parsed.Map(), nil
}

func reviewerExecutable(config map[string]any, provider string) (string, error) {
	if executable, ok := config["reviewer1_executable"].(string); ok && filepath.IsAbs(executable) {
		return executable, nil
	}
	command, ok := reviewerCommands[provider]
	if !ok {
		return "", fmt.Errorf("unknown reviewer provider %q", provider)
	}
	resolved, err := exec.LookPath(command)
	if err == nil {
		absolute, err := filepath.Abs(resolved)
		if err == nil {
			if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
				absolute = canonical
			}
			if filepath.IsAbs(absolute) {
				return absolute, nil
			}
		}
	}
	return "", fmt.Errorf("reviewer1_executable must be an absolute path or `%s` must be available on PATH", command)
}
func RefuseUnsafeFlags(command []string, repositoryRoot string) error {
	if hasFlag(command, "--mode", "plan") && hasFlag(command, "--disable-slash-commands", "") {
		return &CommandRefused{"--disable-slash-commands silently voids --mode plan; refusing unsafe reviewer command"}
	}
	for i, arg := range command {
		if arg != "--add-dir" || i+1 >= len(command) {
			continue
		}
		if repositoryRoot == "" {
			return &CommandRefused{"the repository root is required before granting Reviewer-1 an added directory"}
		}
		candidate, err := resolveReviewerPath(command[i+1])
		if err != nil {
			return &CommandRefused{"--add-dir names an unresolvable path"}
		}
		root, err := resolveReviewerPath(repositoryRoot)
		if err != nil {
			return &CommandRefused{"--add-dir names an unresolvable path"}
		}
		if withinPath(candidate, root) {
			return &CommandRefused{"--add-dir must never grant Reviewer-1 access to the repository under review"}
		}
	}
	return nil
}
func RefuseWorkingDirectory(workingDirectory, repositoryRoot string) error {
	working, err := resolveReviewerPath(workingDirectory)
	if err != nil {
		return err
	}
	repository, err := resolveReviewerPath(repositoryRoot)
	if err != nil {
		return err
	}
	if withinPath(working, repository) {
		return &CommandRefused{"the Reviewer-1 working directory must be outside the repository under review"}
	}
	return nil
}
func hasFlag(command []string, name, value string) bool {
	for i, item := range command {
		if item == name {
			return value == "" || (i+1 < len(command) && command[i+1] == value)
		}
		if strings.HasPrefix(item, name+"=") {
			return value == "" || item == name+"="+value
		}
	}
	return false
}
func resolveReviewerPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	}
	return absolute, nil
}
func flagValue(command []string, name string) string {
	for i, arg := range command {
		if arg == name && i+1 < len(command) {
			return command[i+1]
		}
	}
	return ""
}
func ReviewPrompt(path string) string {
	return "You are Reviewer-1. Read and review only the JSON packet at the exact path below. Do not inspect unrelated files, infer hidden reasoning, or accept an implementer narrative. Return only the supplied JSON schema. Use NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE only when the complete diff and supplied contract establish no material issue. Use UNCERTAIN when the diff or context needed to assess the diff is insufficient. The normal absence of orchestrator validation evidence is not by itself grounds for UNCERTAIN; report validation UNKNOWN without inferring that validation ran.\n\nREVIEW_PACKET_PATH=" + path
}
