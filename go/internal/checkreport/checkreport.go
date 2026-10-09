// Package checkreport implements the read-only Purser installation report.
package checkreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/enforce"
	"purser-hook/internal/hookactivity"
	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
)

type Row struct {
	Check    string  `json:"check"`
	Status   string  `json:"status"`
	Detail   string  `json:"detail"`
	NextStep *string `json:"next_step"`
}

type NextStep struct {
	Number int    `json:"number"`
	Check  string `json:"check"`
	Action string `json:"action"`
}

type Report struct {
	Checks    []Row      `json:"checks"`
	NextSteps []NextStep `json:"next_steps"`
	ExitCode  int        `json:"exit_code"`
}

func row(check, status, detail, next string) Row {
	var step *string
	if next != "" {
		step = &next
	}
	return Row{Check: check, Status: status, Detail: detail, NextStep: step}
}

func Run(projectRoot, pluginRoot string) Report {
	home, _ := os.UserHomeDir()
	return run(projectRoot, pluginRoot, home)
}

func run(projectRoot, pluginRoot, home string) Report {
	p := profile.LoadProfile(projectRoot, filepath.Join(home, ".orchestrator", "config.toml"))
	checks := []Row{
		runRow("Runtime", func() Row {
			return row("Runtime", "OK", RuntimeDetail(pluginRoot), "")
		}),
		runRow("Hook registration", func() Row { return checkRegistration(home) }),
		runRow("Hook activity", func() Row { return hookActivityCheck(home) }),
		runRow("Codex MCP", func() Row { return mcpCheck(home, pluginRoot) }),
		runRow("Codex CLI", codexCheck),
		runRow("Roles", func() Row { return rolesCheck(p) }),
		runRow("Reviewer", func() Row { return reviewerCheck(p, pluginRoot) }),
		runRow("Project", func() Row { return projectCheck(p) }),
		runRow("Quota", quotaCheck),
	}
	report := Report{Checks: checks, NextSteps: []NextStep{}}
	for _, item := range checks {
		if item.Status == "WARN" || item.Status == "FAIL" {
			report.NextSteps = append(report.NextSteps, NextStep{Number: len(report.NextSteps) + 1, Check: item.Check, Action: *item.NextStep})
		}
		if item.Status == "FAIL" {
			report.ExitCode = 1
		}
	}
	return report
}

func hookActivityCheck(home string) Row {
	stateRoot := os.Getenv("PURSER_STATE_DIR")
	if stateRoot == "" {
		if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" {
			stateRoot = filepath.Join(config, "purser")
		} else {
			stateRoot = filepath.Join(home, ".claude", "purser")
		}
	}
	activity := hookactivity.Load(stateRoot)
	if len(activity) == 0 {
		return row("Hook activity", "INFO", "No hook activity recorded yet.", "")
	}
	events := make([]string, 0, len(activity))
	for event := range activity {
		events = append(events, event)
	}
	sort.Strings(events)
	entries := make([]string, 0, len(events))
	now := time.Now()
	for _, event := range events {
		entries = append(entries, event+" "+activityAge(now.Sub(activity[event])))
	}
	return row("Hook activity", "OK", strings.Join(entries, ", ")+". Activity is updated at most hourly.", "")
}

func activityAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
	}
}

func runRow(label string, check func() Row) (result Row) {
	defer func() {
		if failure := recover(); failure != nil {
			result = failedRow(label, failure)
		}
	}()
	result = check()
	if result.Status != "OK" && result.Status != "WARN" && result.Status != "FAIL" && result.Status != "INFO" {
		return failedRow(label, "check returned an invalid result")
	}
	return result
}

func failedRow(label string, failure any) Row {
	return row(label, "WARN", "Check could not run: "+errorText(failure), "Resolve the reported check error and run `/purser:check` again.")
}

func errorText(value any) string {
	if err, ok := value.(error); ok {
		if err.Error() != "" {
			return err.Error()
		}
		return fmt.Sprintf("%T", err)
	}
	return fmt.Sprint(value)
}

func registrationCheck(home string) Row {
	path := filepath.Join(home, ".claude", "settings.json")
	settings, err := readObject(path)
	if err != nil {
		return row("Hook registration", "WARN", "Could not read "+path+": "+err.Error(), "Check that ~/.claude/settings.json is readable JSON and contains the plugin or manual hook registration.")
	}
	manual := containsManualHook(settings["hooks"])
	pluginKeys := []string{}
	if enabled, ok := settings["enabledPlugins"].(map[string]any); ok {
		for key, value := range enabled {
			if strings.HasPrefix(key, "purser@") && value == true {
				pluginKeys = append(pluginKeys, key)
			}
		}
	}
	sort.Strings(pluginKeys)
	plugin := len(pluginKeys) > 0
	if manual && plugin {
		return row("Hook registration", "FAIL", "double registration: manual Purser hooks and enabled plugin `"+pluginKeys[0]+"` are both present.", "Remove the manual hook entries from ~/.claude/settings.json; see docs/PLUGIN.md.")
	}
	if !manual && !plugin {
		return row("Hook registration", "FAIL", "Neither manual Purser hooks nor an enabled Purser plugin were found.", "Install and enable `purser@<marketplace>`, or register the manual Purser hooks; see docs/PLUGIN.md.")
	}
	if plugin {
		return row("Hook registration", "OK", "Purser plugin enabled as `"+pluginKeys[0]+"`; no manual hooks found.", "")
	}
	return row("Hook registration", "OK", "Manual Purser hooks found; no enabled Purser plugin found.", "")
}

var checkRegistration = registrationCheck

func containsManualHook(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for _, item := range value {
			if containsManualHook(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if containsManualHook(item) {
				return true
			}
		}
	case string:
		value = strings.ToLower(strings.ReplaceAll(value, `\`, "/"))
		return strings.Contains(value, ".orchestrator") || strings.Contains(value, "hooks/orchestrator/hook.py")
	}
	return false
}

func mcpCheck(home, pluginRoot string) Row {
	userPath := filepath.Join(home, ".claude.json")
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	pluginPath := filepath.Join(pluginRoot, "plugin", "mcp.json")
	user, err := readOptionalObject(userPath)
	if err != nil {
		return mcpFailure(err)
	}
	settings, err := readOptionalObject(settingsPath)
	if err != nil {
		return mcpFailure(err)
	}
	plugin, err := readObject(pluginPath)
	if err != nil {
		return mcpFailure(err)
	}
	userServer := nestedObject(user, "mcpServers", "codex-main")
	pluginServer := nestedObject(plugin, "mcpServers", "codex-main")
	active := false
	if enabled, ok := settings["enabledPlugins"].(map[string]any); ok {
		for key, value := range enabled {
			if strings.HasPrefix(key, "purser@") && value == true {
				active = true
			}
		}
	}
	pluginActive := active && pluginServer != nil
	userActive := userServer != nil
	userCommand := commandValue(userServer)
	pluginCommand := commandValue(pluginServer)
	switch {
	case userActive && pluginActive:
		return row("Codex MCP", "WARN", "duplicate `codex-main`: user command `"+userCommand+"` and enabled plugin command `"+pluginCommand+"`.", "Remove the manual `codex-main` entry from ~/.claude.json; see docs/PLUGIN.md.")
	case userActive:
		return row("Codex MCP", "OK", "user-level `codex-main` command: `"+userCommand+"`.", "")
	case pluginActive:
		return row("Codex MCP", "OK", "plugin `codex-main` command: `"+pluginCommand+"`.", "")
	case pluginServer != nil:
		return row("Codex MCP", "INFO", "plugin `codex-main` is bundled but the Purser plugin is not enabled.", "")
	default:
		return row("Codex MCP", "INFO", "No active `codex-main` MCP registration found.", "")
	}
}

func mcpFailure(err error) Row {
	return row("Codex MCP", "WARN", "Could not inspect MCP registration: "+err.Error(), "Check that ~/.claude.json and plugin/mcp.json are readable JSON.")
}

func commandValue(server map[string]any) string {
	if server == nil {
		return ""
	}
	if value, ok := server["command"]; ok {
		return fmt.Sprint(value)
	}
	return "(command missing)"
}

func codexCheck() Row {
	executable, err := lookPath("codex")
	if err != nil {
		return row("Codex CLI", "FAIL", "`codex` was not found on PATH.", "Install the Codex CLI and ensure `codex` resolves on PATH.")
	}
	output, stderr, code, err := runCodex(executable)
	output = strings.TrimSpace(output)
	if output == "" {
		output = strings.TrimSpace(stderr)
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return row("Codex CLI", "WARN", fmt.Sprintf("Could not run `%s --version`: %s", executable, err.Error()), "Verify that the Codex CLI executable is runnable and responds within 10 seconds.")
		}
		return row("Codex CLI", "WARN", fmt.Sprintf("`%s --version` exited %d: %s.", executable, code, fallback(output, "no version output")), "Verify the Codex CLI installation and that it can run `codex --version`.")
	}
	match := codexVersion.FindStringSubmatch(output)
	if match == nil {
		return row("Codex CLI", "WARN", "Could not parse version from `"+output+"`.", "Update the Codex CLI and verify that `codex --version` reports a version.")
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major == 0 && minor < 156 {
		return row("Codex CLI", "WARN", output+"; gpt-6 routes require Codex CLI 0.156 or newer.", "Update the Codex CLI to version 0.156 or newer for gpt-6 routes.")
	}
	return row("Codex CLI", "OK", output+"; "+executable, "")
}

var lookPath = exec.LookPath
var runCodex = func(executable string) (string, string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--version")
	proc.Hide(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout.String(), stderr.String(), 0, fmt.Errorf("Command '['%s', '--version']' timed out after 10 seconds", executable)
	}
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code, err
}

var codexVersion = regexp.MustCompile(`(?:^|[^0-9])([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

func fallback(value, alternative string) string {
	if value == "" {
		return alternative
	}
	return value
}

func rolesCheck(p profile.ResolvedProfile) Row {
	detail := fmt.Sprintf("orchestrator=%s; coder=%s; reviewer=%s", p.Roles.Orchestrator, p.Roles.Coder, p.Roles.Reviewer)
	if p.Roles.Reviewer == "none" {
		return row("Roles", "INFO", detail+"; every call is FULL review", "")
	}
	return row("Roles", "OK", detail, "")
}

func reviewerCheck(p profile.ResolvedProfile, pluginRoot string) Row {
	if p.Roles.Reviewer == "none" {
		return row("Reviewer", "INFO", "No independent reviewer is configured; every call is FULL review.", "")
	}
	executable, source := "", "engine config"
	if p.ReviewerExecutable != nil && *p.ReviewerExecutable != "" {
		executable, source = *p.ReviewerExecutable, "user profile override"
	}
	if executable == "" {
		config, err := readObject(filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json"))
		if err != nil {
			panic(err)
		}
		executable, _ = config["reviewer1_executable"].(string)
	}
	if executable == "" {
		return row("Reviewer", "WARN", "No reviewer1_executable is configured; reviewer unavailable; every call will be FULL review.", "Set reviewer.executable in ~/.orchestrator/config.toml or reviewer1_executable in the engine config.")
	}
	path := expandHome(executable)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return row("Reviewer", "WARN", source+" reviewer executable does not exist: "+path+"; reviewer unavailable; every call will be FULL review.", "Install or restore the configured reviewer executable, or update its configured path.")
	} else if err != nil {
		panic(err)
	}
	return row("Reviewer", "OK", source+" reviewer executable exists: "+path, "")
}

func projectCheck(p profile.ResolvedProfile) Row {
	detail := fmt.Sprintf("trusted=%s; profile present=%s; [[checks]]=%d", pythonBool(p.Trusted), pythonBool(p.Configured), len(p.Checks))
	if len(p.Warnings) > 0 {
		detail += "; warnings: " + strings.Join(p.Warnings, " | ")
		return row("Project", "WARN", detail, "Review the listed profile warnings and correct .orchestrator/profile.toml or ~/.orchestrator/config.toml.")
	}
	return row("Project", "OK", detail, "")
}

func pythonBool(value bool) string {
	if value {
		return "True"
	}
	return "False"
}

func quotaCheck() Row {
	thread := newestRolloutThread()
	var quota map[string]any
	if thread != "" {
		quota = enforce.ReadQuota(thread)
	}
	if len(quota) == 0 {
		return row("Quota", "INFO", "Quota data is unavailable for the newest rollout.", "")
	}
	values := []string{}
	for _, item := range []struct{ label, key string }{{"5h", "5h_used_pct"}, {"7d", "7d_used_pct"}} {
		if value, ok := quota[item.key]; ok {
			values = append(values, item.label+" used "+pythonFloat(value)+"%")
		}
	}
	if len(values) == 0 {
		return row("Quota", "INFO", "Newest rollout has no 5h/7d quota values.", "")
	}
	return row("Quota", "OK", strings.Join(values, "; "), "")
}

func newestRolloutThread() string {
	base := os.Getenv("CODEX_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".codex")
	}
	sessions := filepath.Join(base, "sessions")
	var newest string
	var newestTime int64
	_ = filepath.WalkDir(sessions, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if newest == "" || info.ModTime().UnixNano() > newestTime {
			newest, newestTime = entry.Name(), info.ModTime().UnixNano()
		}
		return nil
	})
	if newest == "" {
		return ""
	}
	match := rolloutID.FindStringSubmatch(newest)
	if match == nil {
		return ""
	}
	return match[1]
}

var rolloutID = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F-]{27,})\.jsonl$`)

func pythonFloat(value any) string {
	if number, ok := value.(float64); ok {
		text := strconv.FormatFloat(number, 'f', -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		return text
	}
	return fmt.Sprint(value)
}

func pluginVersion(pluginRoot string) string {
	manifest, err := readObject(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"))
	if err != nil {
		return "unknown"
	}
	version, ok := manifest["version"].(string)
	if !ok || version == "" {
		return "unknown"
	}
	return version
}

func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("configuration root is not an object")
	}
	return object, nil
}

func readOptionalObject(path string) (map[string]any, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	return readObject(path)
}

func nestedObject(value map[string]any, parent, child string) map[string]any {
	items, ok := value[parent].(map[string]any)
	if !ok {
		return nil
	}
	result, _ := items[child].(map[string]any)
	return result
}

func expandHome(value string) string {
	if value == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, value[2:])
	}
	return value
}

func RenderMarkdown(report Report) string {
	lines := []string{"| Check | Status | Detail |", "| --- | --- | --- |"}
	for _, item := range report.Checks {
		detail := strings.ReplaceAll(strings.ReplaceAll(item.Detail, "|", `\|`), "\n", " ")
		lines = append(lines, fmt.Sprintf("| %s | %s | %s |", item.Check, item.Status, detail))
	}
	if len(report.NextSteps) > 0 {
		lines = append(lines, "", "Next steps:")
		for _, step := range report.NextSteps {
			lines = append(lines, fmt.Sprintf("%d. **%s:** %s", step.Number, step.Check, step.Action))
		}
	}
	return strings.Join(lines, "\n")
}

func WriteJSON(writer io.Writer, report Report) error {
	var compact bytes.Buffer
	encoder := json.NewEncoder(&compact)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(report); err != nil {
		return err
	}
	encoded := compact.Bytes()
	pretty := make([]byte, 0, len(encoded)+len(encoded)/8)
	quoted, escaped := false, false
	for _, value := range encoded[:len(encoded)-1] {
		if quoted {
			pretty = append(pretty, value)
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				quoted = false
			}
			continue
		}
		if value == '"' {
			quoted = true
			pretty = append(pretty, value)
			continue
		}
		pretty = append(pretty, value)
		if value == ',' || value == ':' {
			pretty = append(pretty, ' ')
		}
	}
	pretty = append(pretty, '\n')
	written, err := writer.Write(pretty)
	if err == nil && written != len(pretty) {
		return io.ErrShortWrite
	}
	return err
}

// RuntimeTarget is supplied by the standard library and used for the Runtime row.
var runtimeGOOS, runtimeGOARCH = runtime.GOOS, runtime.GOARCH

// RuntimeDetail returns the detail shown by the Runtime row.
func RuntimeDetail(pluginRoot string) string {
	return "purser-hook " + pluginVersion(pluginRoot) + " (" + runtimeGOOS + "/" + runtimeGOARCH + ")"
}
