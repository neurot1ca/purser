package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

const defaultPacketBudget = 200000

const (
	maxReviewerStdoutBytes = 16 * 1024 * 1024
	maxReviewerStderrBytes = 2048
)

var requiredBridgeArguments = []string{"task_id", "phase", "route", "model", "effort", "repo_root", "delta", "classification"}
var optionalBridgeArguments = map[string]func(any) bool{
	"thread_id": stringOrNil, "project_key": stringOrNil, "state_root": stringOrNil,
	"validation_summary": stringOrNil, "task_objective": stringOrNil, "acceptance_criteria": stringOrNil,
	"reviewer_executable": stringOrNil, "authority_sources": func(v any) bool { return v == nil || isSlice(v) },
	"reviewer_agent": isString, "reviewer_model": stringOrNil,
	"reviewer_effort": func(v any) bool { value, ok := v.(string); return ok && claudeReviewerEfforts[value] },
	"reviewer_timeout_seconds": func(v any) bool {
		if v == nil {
			return true
		}
		_, ok := finiteNumber(v)
		return ok
	},
}

var reviewSchema = map[string]any{
	"type": "object", "additionalProperties": false,
	"required": []string{"result", "validation", "findings"},
	"properties": map[string]any{
		"result":     map[string]any{"type": "string", "enum": []string{NoMaterialFinding, ChangesRequired, Uncertain, Reviewer1Unavailable}},
		"validation": map[string]any{"type": "string", "enum": []string{"PASS", "FAIL", "UNKNOWN"}},
		"findings":   map[string]any{"type": "array", "maxItems": 20, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"code", "message"}, "properties": map[string]any{"code": map[string]any{"type": "string", "minLength": 1, "maxLength": 120}, "message": map[string]any{"type": "string", "minLength": 1, "maxLength": 2000}}}},
	},
}

func stringOrNil(v any) bool { return v == nil || isString(v) }
func isString(v any) bool    { _, ok := v.(string); return ok }
func isSlice(v any) bool {
	switch v.(type) {
	case []any, []string:
		return true
	}
	return false
}

func parseBridgeArguments(args map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("stdin must contain valid JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("stdin must contain valid JSON: %w", err)
	}
	kwargs, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("input must be a JSON object")
	}
	allowed := map[string]bool{}
	for _, key := range requiredBridgeArguments {
		allowed[key] = true
	}
	for key := range optionalBridgeArguments {
		allowed[key] = true
	}
	unknown := make([]string, 0)
	for key := range kwargs {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sortStrings(unknown)
		return nil, fmt.Errorf("unknown key: %s", unknown[0])
	}
	for _, key := range requiredBridgeArguments {
		if _, ok := kwargs[key]; !ok {
			return nil, fmt.Errorf("missing key: %s", key)
		}
	}
	for _, key := range []string{"task_id", "phase", "route", "model", "effort", "repo_root"} {
		if !isString(kwargs[key]) {
			return nil, fmt.Errorf("%s must be a string", key)
		}
	}
	if _, ok := kwargs["delta"].(map[string]any); !ok {
		return nil, errors.New("delta must be an object")
	}
	if kwargs["classification"] != nil {
		if _, ok := kwargs["classification"].(map[string]any); !ok {
			return nil, errors.New("classification must be an object or null")
		}
	}
	for key, valid := range optionalBridgeArguments {
		if value, ok := kwargs[key]; ok && !valid(value) {
			return nil, fmt.Errorf("%s has the wrong type", key)
		}
	}
	if value, ok := kwargs["reviewer_timeout_seconds"]; ok && value != nil {
		n, _ := finiteNumber(value)
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, errors.New("reviewer_timeout_seconds must be finite")
		}
	}
	return kwargs, nil
}

func finiteNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func bridgeOutcome(value any) (BridgeOutcome, error) {
	switch result := value.(type) {
	case CompletedReview:
		attributes := result.Map()
		attributes["usage"] = result.Usage
		return BridgeOutcome{Outcome: "completed", Attributes: attributes}, nil
	case UnavailableReview:
		return BridgeOutcome{Outcome: "unavailable", Attributes: result.Map(), Diagnostics: result.Diagnostics}, nil
	case IncompleteReview:
		return BridgeOutcome{Outcome: "incomplete", Attributes: result.Map()}, nil
	default:
		return BridgeOutcome{}, errors.New("review returned an unknown outcome")
	}
}

func bridgePanic(value any, args map[string]any) (BridgeOutcome, error) {
	name := "NoneType"
	if value != nil {
		t := reflect.TypeOf(value)
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		name = t.Name()
		if name == "" {
			name = t.String()
		}
	}
	model, _ := args["reviewer_model"].(string)
	if model == "" {
		model = "UNAVAILABLE"
	}
	provider, _ := args["reviewer_agent"].(string)
	if provider == "" {
		provider = "agy"
	}
	attrs := map[string]any{"reason": "REVIEWER_BRIDGE_EXCEPTION", "review_status": "UNAVAILABLE", "available": false, "provider": provider, "result": Reviewer1Unavailable, "validation": "UNKNOWN", "findings": []map[string]string{FindingMap("REVIEWER_BRIDGE_EXCEPTION", "Reviewer-1 failed unexpectedly ("+name+").")}, "error_code": "REVIEWER_BRIDGE_EXCEPTION", "reviewer1_model_requested": model, "served_model_attestation": "UNAVAILABLE"}
	return BridgeOutcome{Outcome: "unavailable", Attributes: attrs}, &BridgeError{Code: "REVIEWER_BRIDGE_EXCEPTION", Err: fmt.Errorf("reviewer panic (%s)", name)}
}

func nativeReview(ctx context.Context, pluginRoot string, args map[string]any) (BridgeOutcome, error) {
	kwargs, err := parseBridgeArguments(args)
	if err != nil {
		return BridgeOutcome{}, &BridgeError{Code: "INVALID_INPUT", Err: err}
	}
	var materials map[string]any
	delta := kwargs["delta"].(map[string]any)
	classification, _ := kwargs["classification"].(map[string]any)
	optionalString := func(key string) *string {
		value, ok := kwargs[key].(string)
		if !ok {
			return nil
		}
		return &value
	}
	var authority []any
	if raw, ok := kwargs["authority_sources"].([]any); ok {
		authority = raw
	} else if raw, ok := kwargs["authority_sources"].([]string); ok {
		for _, value := range raw {
			authority = append(authority, value)
		}
	}
	materials, err = BuildMaterials(kwargs["task_id"].(string), kwargs["repo_root"].(string), delta, classification, optionalString("validation_summary"), optionalString("task_objective"), optionalString("acceptance_criteria"), authority)
	config, configErr := loadNativeConfig(pluginRoot)
	if configErr != nil {
		provider, _ := kwargs["reviewer_agent"].(string)
		if provider == "" {
			provider = "agy"
		}
		return bridgeOutcome(Unavailable("REVIEWER_CONFIG_INVALID", "Reviewer-1 configuration was invalid ("+errorType(configErr)+"): "+configErr.Error(), map[string]any{"reviewer1_model_requested": pinnedModel}, provider))
	}
	agent, _ := kwargs["reviewer_agent"].(string)
	if agent == "" {
		agent = "agy"
	}
	model, _ := kwargs["reviewer_model"].(string)
	if model == "" {
		model = RequestedModel(config)
	}
	if model == "UNAVAILABLE" {
		model = pinnedModel
	}
	config["reviewer1_model_requested"], config["reviewer1_agent"] = model, agent
	if effort, ok := kwargs["reviewer_effort"].(string); ok && effort != "" {
		config["reviewer1_effort"] = effort
	}
	if executable, ok := kwargs["reviewer_executable"].(string); ok && executable != "" {
		config["reviewer1_executable"] = executable
	}
	if timeout, ok := kwargs["reviewer_timeout_seconds"]; ok && timeout != nil {
		n, _ := finiteNumber(timeout)
		config["reviewer1_timeout_seconds"] = n
	}
	if err := ValidateConfig(config); err != nil {
		return bridgeOutcome(Unavailable("REVIEWER_CONFIG_INVALID", "Reviewer-1 configuration was invalid (ValueError): "+err.Error(), config, agent))
	}
	if materials == nil || err != nil {
		omitted := PacketOmitted{"REVIEW_PACKET_INCOMPLETE", "The complete delegated review packet could not be built (" + errorType(err) + ": " + trimError(err) + "); no model call was made.", []string{"delegated_diff"}}
		return bridgeOutcome(IncompletePacket(omitted, config, agent))
	}
	outcome, err := reviewMaterials(ctx, materials, config, kwargs)
	if outcome.Attributes != nil {
		// Host-only metadata; it never enters the reviewer packet or worker prompt.
		outcome.Attributes["review_diff"] = materials["delegated_diff"]
	}
	return outcome, err
}

func loadNativeConfig(pluginRoot string) (map[string]any, error) {
	path := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&config); err != nil {
		return nil, err
	}
	return config, nil
}

func errorType(err error) string {
	if err == nil {
		return "ValueError"
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() != "" {
		return t.Name()
	}
	return t.String()
}
func trimError(err error) string {
	if err == nil {
		return "unknown error"
	}
	message := err.Error()
	if len(message) > 200 {
		return message[:200]
	}
	return message
}

func reviewMaterials(ctx context.Context, materials map[string]any, config map[string]any, args map[string]any) (BridgeOutcome, error) {
	provider, _ := config["reviewer1_agent"].(string)
	if provider == "" {
		provider = "agy"
	}
	adapter, adapterErr := ReviewerAdapter(provider)
	if adapterErr != nil {
		return bridgeOutcome(Unavailable("REVIEWER_ADAPTER_UNAVAILABLE", "No reviewer adapter is registered for "+provider+".", config, provider))
	}
	budget := defaultPacketBudget
	if n, ok := finiteNumber(config["reviewer1_packet_max_bytes"]); ok && n > 0 {
		budget = int(n)
	}
	packet := BuildReviewPacket(materials, budget)
	if omitted, ok := packet.(PacketOmitted); ok {
		return bridgeOutcome(IncompletePacket(omitted, config, provider))
	}
	ready := packet.(PacketReady)
	seconds, ok := finiteNumber(config["reviewer1_timeout_seconds"])
	if !ok || !isFinite(seconds) || seconds <= 0 {
		return bridgeOutcome(Unavailable("REVIEWER_CONFIG_INVALID", "Reviewer-1 configuration was invalid (ValueError): reviewer1_timeout_seconds must be positive", config, provider))
	}
	if timeout, exists := args["reviewer_timeout_seconds"]; exists && timeout != nil {
		seconds, _ = finiteNumber(timeout)
	}
	if seconds <= 0 {
		return bridgeOutcome(Unavailable("REVIEWER_CONFIG_INVALID", "Reviewer-1 configuration was invalid (ValueError): reviewer1_timeout_seconds must be positive", config, provider))
	}
	remaining := time.Duration(seconds * float64(time.Second))
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < remaining {
		remaining = time.Until(deadline)
	}
	if remaining <= 0 {
		return bridgeOutcome(Unavailable("REVIEWER_TIMEOUT", "Reviewer-1 exceeded its bounded timeout.", config, provider))
	}
	root := fmt.Sprint(args["repo_root"])
	working, err := runtimeTempDir("reviewer1-")
	if err != nil {
		return bridgeOutcome(Unavailable("REVIEWER_WORKDIR_UNAVAILABLE", "The isolated Reviewer-1 working directory failed ("+errorType(err)+").", config, provider))
	}
	defer os.RemoveAll(working)
	if provider == "opencode" {
		working, err = os.MkdirTemp(filepath.Dir(working), "reviewer1-opencode-")
		if err != nil {
			return bridgeOutcome(Unavailable("REVIEWER_WORKDIR_UNAVAILABLE", "The isolated Reviewer-1 working directory failed ("+errorType(err)+").", config, provider))
		}
	}
	defer os.RemoveAll(working)
	if err := RefuseWorkingDirectory(working, root); err != nil {
		return bridgeOutcome(Unavailable(ResponseInvalid, "Reviewer-1 response or configuration was invalid (ValueError): "+err.Error(), config, provider))
	}
	packetName := ""
	if provider == "opencode" {
		packetName = "review-packet.json"
	}
	packetPath, err := WritePacket(working, packetName, ready.Encoded)
	if err != nil {
		return bridgeOutcome(Unavailable(ResponseInvalid, "Reviewer-1 response or configuration was invalid ("+errorType(err)+"): "+err.Error(), config, provider))
	}
	defer os.Remove(packetPath)
	if err := adapter.ValidateModel(config["reviewer1_model_requested"]); err != nil {
		return bridgeOutcome(Unavailable(ResponseInvalid, "Reviewer-1 response or configuration was invalid (ValueError): "+err.Error(), config, provider))
	}
	options := CommandOptions{WorkingDirectory: working, RepositoryRoot: root}
	prompt := ReviewPrompt(packetPath)
	if provider == "agy" || provider == "codex" {
		prompt = reviewerPromptWithPacket(ready.Encoded)
	}
	command, err := adapter.BuildCommand(prompt, reviewSchema, config, options)
	if err != nil {
		code := ResponseInvalid
		var refused *CommandRefused
		if errors.As(err, &refused) {
			code = "REVIEWER_COMMAND_REFUSED"
		}
		return bridgeOutcome(Unavailable(code, "Reviewer-1 response or configuration was invalid ("+errorType(err)+"): "+err.Error(), config, provider))
	}
	stdout, stderr, elapsed, retries, callErr, exitCode, evidence := invokePacket(ctx, adapter, command, prompt, working, remaining)
	if callErr != "" {
		unavailable := Unavailable(callErr, reviewerFailureMessage(callErr, retries, exitCode), config, provider)
		unavailable.ElapsedSeconds = nativeFloatPtr(elapsed.Seconds())
		unavailable.Diagnostics = reviewerDiagnostics(evidence, callErr, elapsed, exitCode, "")
		stdoutTail, _ := evidence["_diagnostic_stdout_tail"].(string)
		stderrHead, _ := evidence["_diagnostic_stderr_head"].(string)
		stderrTail, _ := evidence["_diagnostic_stderr_tail"].(string)
		executablePath, _ := evidence["_diagnostic_executable_path"].(string)
		argvFlags, _ := evidence["_diagnostic_argv_flags"].([]string)
		pathHead, _ := evidence["_diagnostic_path_head"].(string)
		_ = writeReviewerDiagnostic(provider, RequestedModel(config), callErr, exitCode, elapsed, retries, "", stdout, stdoutTail, stderrHead, stderrTail, executablePath, argvFlags, working, pathHead)
		if stderr != "" {
			unavailable.StderrTail = StderrTail(stderr)
		}
		return bridgeOutcome(unavailable)
	}
	parsed, err := adapter.ParseEnvelope(string(stdout))
	if err != nil {
		code := ResponseInvalid
		message := fmt.Sprintf("Reviewer-1 response or configuration was invalid (%s): %s", errorType(err), err.Error())
		if errors.Is(err, errAgyToolDenied) {
			code = "REVIEWER_TOOL_DENIED"
			message = "Reviewer-1 could not produce a review because a required tool was denied."
		}
		unavailable := Unavailable(code, message+retrySuffix(retries), config, provider)
		unavailable.RawStdout = textPtr(lastBytes(stdout, 262144))
		detail := err.Error()
		if len(detail) > 200 {
			detail = detail[:200]
		}
		unavailable.DecoderDetail = &detail
		unavailable.ElapsedSeconds = nativeFloatPtr(elapsed.Seconds())
		unavailable.Diagnostics = reviewerDiagnostics(evidence, "", elapsed, 0, detail)
		stdoutTail, _ := evidence["_diagnostic_stdout_tail"].(string)
		stderrHead, _ := evidence["_diagnostic_stderr_head"].(string)
		stderrTail, _ := evidence["_diagnostic_stderr_tail"].(string)
		executablePath, _ := evidence["_diagnostic_executable_path"].(string)
		argvFlags, _ := evidence["_diagnostic_argv_flags"].([]string)
		pathHead, _ := evidence["_diagnostic_path_head"].(string)
		_ = writeReviewerDiagnostic(provider, RequestedModel(config), ResponseInvalid, 0, elapsed, retries, detail, stdout, stdoutTail, stderrHead, stderrTail, executablePath, argvFlags, working, pathHead)
		return bridgeOutcome(unavailable)
	}
	response := parsed["response"].(map[string]any)
	if response["result"] == Reviewer1Unavailable {
		unavailable := Unavailable("REVIEWER_MODEL_REPORTED_UNAVAILABLE", "Reviewer-1 returned REVIEWER1_UNAVAILABLE."+retrySuffix(retries), config, provider)
		unavailable.ElapsedSeconds = nativeFloatPtr(elapsed.Seconds())
		unavailable.Diagnostics = reviewerDiagnostics(evidence, "", elapsed, 0, "reviewer model reported unavailable")
		if stderr != "" {
			unavailable.StderrTail = StderrTail(stderr)
		}
		return bridgeOutcome(unavailable)
	}
	result, _ := response["result"].(string)
	findings, _ := response["findings"].([]any)
	completed := CompletedReview{Result: result, Validation: response["validation"].(string), Findings: findingsFromAny(findings), ConversationID: parsed["conversation_id"].(string), Reviewer1ModelRequested: RequestedModel(config), DurationSeconds: parsed["duration_seconds"].(*float64), Usage: parsed["usage"].(map[string]any), Provider: provider, ServedModelAttestation: fmt.Sprint(parsed["served_model_attestation"]), RetryCount: retries, RetryRecovered: retries > 0}
	if ready.PartialCoverage {
		completed.Result, completed.Validation = Uncertain, "UNKNOWN"
		completed.Findings = append(completed.Findings, Finding{Code: "REVIEW_PACKET_PARTIAL_COVERAGE", Message: "Reviewer-1 received a bounded diff excerpt; the complete delegated change requires FULL Claude review."})
	}
	return bridgeOutcome(completed)
}

func reviewerDiagnostics(evidence map[string]any, failure string, elapsed time.Duration, exitCode int, parseError string) map[string]any {
	diagnostics := map[string]any{"reviewer_elapsed_seconds": elapsed.Seconds()}
	for key, value := range evidence {
		if strings.HasPrefix(key, "_diagnostic_") {
			continue
		}
		diagnostics[key] = value
	}
	if failure == "REVIEWER_TIMEOUT" {
		diagnostics["reviewer_exit"] = fmt.Sprintf("timeout after %.3fs", elapsed.Seconds())
	} else if exitCode != 0 {
		diagnostics["reviewer_exit_code"] = exitCode
	}
	if parseError != "" {
		diagnostics["reviewer_parse_error"] = parseError
	}
	return diagnostics
}

func isFinite(value float64) bool           { return !math.IsInf(value, 0) && !math.IsNaN(value) }
func nativeFloatPtr(value float64) *float64 { return &value }
func textPtr(value string) *string          { return &value }
func findingsFromAny(values []any) []Finding {
	out := make([]Finding, 0, len(values))
	for _, raw := range values {
		if finding, ok := raw.(map[string]any); ok {
			out = append(out, Finding{fmt.Sprint(finding["code"]), fmt.Sprint(finding["message"])})
		}
	}
	return out
}
func lastBytes(value []byte, limit int) string {
	if len(value) > limit {
		value = value[len(value)-limit:]
	}
	return strings.ToValidUTF8(string(value), "�")
}
func retrySuffix(retries int) string {
	if retries > 0 {
		return " (after 1 retry)"
	}
	return ""
}
func reviewerFailureMessage(code string, retries, exitCode int) string {
	switch code {
	case "REVIEWER_TIMEOUT":
		return "Reviewer-1 exceeded its bounded timeout." + retrySuffix(retries)
	case "REVIEWER_EXECUTABLE_MISSING":
		return "The configured Reviewer-1 executable was not found." + retrySuffix(retries)
	case "REVIEWER_PROCESS_FAILED":
		return fmt.Sprintf("Reviewer-1 exited non-zero (%d).", exitCode) + retrySuffix(retries)
	default:
		return "Reviewer-1 response or configuration was invalid (error)." + retrySuffix(retries)
	}
}

func diagnosticReviewerArgv(command []string) []string {
	argv := append([]string(nil), command...)
	for i, arg := range argv {
		if strings.Contains(arg, "You are Reviewer-1.") {
			argv[i] = fmt.Sprintf("<omitted %d bytes>", len(arg))
		}
		if arg == "--json-schema" && i+1 < len(argv) {
			argv[i+1] = fmt.Sprintf("<omitted %d bytes>", len(argv[i+1]))
		}
	}
	return argv
}

func invokePacket(parent context.Context, adapter Adapter, command []string, prompt, cwd string, timeout time.Duration) ([]byte, string, time.Duration, int, string, int, map[string]any) {
	started := time.Now()
	retried := 0
	for {
		remaining := timeout - time.Since(started)
		if remaining <= 0 {
			return nil, "", time.Since(started), retried, "REVIEWER_TIMEOUT", 0, nil
		}
		ctx, cancel := context.WithTimeout(parent, remaining)
		cmd := proc.Hide(exec.Command(command[0], command[1:]...))
		cmd.Dir = cwd
		cmd.Env = os.Environ()
		if adapter.Provider() == "agy" && hasFlag(command, "--input-format", "stream-json") {
			input, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": prompt}})
			cmd.Stdin = bytes.NewReader(append(input, '\n'))
		} else if adapter.Provider() == "codex" && len(command) > 1 && command[len(command)-1] == "-" {
			cmd.Stdin = bytes.NewReader([]byte(prompt))
		}
		for key, value := range adapter.Environment(command) {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		pathValue := ""
		for _, item := range cmd.Env {
			key, value, ok := strings.Cut(item, "=")
			if ok && strings.EqualFold(key, "PATH") {
				pathValue = value
			}
		}
		pathRunes := []rune(pathValue)
		if len(pathRunes) > 600 {
			pathRunes = pathRunes[:600]
		}
		executablePath := cmd.Path
		if absolute, err := filepath.Abs(executablePath); err == nil {
			executablePath = absolute
		}
		var stdout, stderr boundedBuffer
		stdout.limit = maxReviewerStdoutBytes
		stderr.limit, stderr.tail = maxReviewerStderrBytes, true
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		rootExit, err := runSupervised(ctx, cmd.Args, cmd.Dir, cmd.Env, cmd.Stdin, &stdout, &stderr, adapter.LaunchOptions())
		elapsed := time.Since(started)
		stdoutHead, stdoutTail := stdout.diagnosticParts()
		stderrHead, stderrTail := stderr.diagnosticParts()
		evidence := map[string]any{
			"reviewer_stdout": stdout.evidence(), "reviewer_stderr": stderr.evidence(),
			"_diagnostic_stdout_head": stdoutHead, "_diagnostic_stdout_tail": stdoutTail,
			"_diagnostic_stderr_head": stderrHead, "_diagnostic_stderr_tail": stderrTail,
			"_diagnostic_executable_path": executablePath, "_diagnostic_argv_flags": diagnosticReviewerArgv(command),
			"_diagnostic_path_head": string(pathRunes),
		}
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			if stdout.overflowed {
				return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, ResponseInvalid, 0, evidence
			}
			return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, "", 0, evidence
		}
		if timedOut {
			return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, "REVIEWER_TIMEOUT", 0, evidence
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, "REVIEWER_EXECUTABLE_MISSING", 0, evidence
		}
		if errors.Is(err, errTeardownUnconfirmed) {
			return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, "REVIEWER_PROCESS_FAILED", rootExit, evidence
		}
		if rootExit == 0 {
			return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, ResponseInvalid, 0, evidence
		}
		left := timeout - elapsed
		if retried == 0 && elapsed < time.Duration(fastProcessFailureRetrySeconds*float64(time.Second)) && left > time.Duration(minRetryRemainingSeconds*float64(time.Second)) {
			retried = 1
			continue
		}
		return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, "REVIEWER_PROCESS_FAILED", rootExit, evidence
	}
}

type boundedBuffer struct {
	bytes.Buffer
	limit          int
	tail           bool
	overflowed     bool
	evidenceHead   []byte
	evidenceTail   []byte
	evidenceBytes  int
	diagnosticHead []byte
	diagnosticTail []byte
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	const evidenceLimit = 2048
	if len(b.diagnosticHead) < reviewerDiagnosticBytes {
		b.diagnosticHead = append(b.diagnosticHead, value[:min(len(value), reviewerDiagnosticBytes-len(b.diagnosticHead))]...)
	}
	if len(value) >= reviewerDiagnosticBytes {
		b.diagnosticTail = append(b.diagnosticTail[:0], value[len(value)-reviewerDiagnosticBytes:]...)
	} else {
		if overflow := len(b.diagnosticTail) + len(value) - reviewerDiagnosticBytes; overflow > 0 {
			b.diagnosticTail = b.diagnosticTail[overflow:]
		}
		b.diagnosticTail = append(b.diagnosticTail, value...)
	}
	if len(b.evidenceHead) < evidenceLimit {
		b.evidenceHead = append(b.evidenceHead, value[:min(len(value), evidenceLimit-len(b.evidenceHead))]...)
	}
	b.evidenceBytes += len(value)
	if len(value) >= evidenceLimit {
		b.evidenceTail = append(b.evidenceTail[:0], value[len(value)-evidenceLimit:]...)
	} else {
		if overflow := len(b.evidenceTail) + len(value) - evidenceLimit; overflow > 0 {
			b.evidenceTail = b.evidenceTail[overflow:]
		}
		b.evidenceTail = append(b.evidenceTail, value...)
	}
	if b.tail {
		if len(value) >= b.limit {
			b.Reset()
			_, _ = b.Buffer.Write(value[len(value)-b.limit:])
			return len(value), nil
		}
		if overflow := b.Len() + len(value) - b.limit; overflow > 0 {
			b.Next(overflow)
		}
		_, _ = b.Buffer.Write(value)
		return len(value), nil
	}
	if b.limit <= b.Len() {
		b.overflowed = true
		return len(value), nil
	}
	keep := value
	if remaining := b.limit - b.Len(); len(keep) > remaining {
		keep = keep[:remaining]
		b.overflowed = true
	}
	_, _ = b.Buffer.Write(keep)
	return len(value), nil
}

func (b *boundedBuffer) diagnosticParts() (string, string) {
	return strings.ToValidUTF8(string(b.diagnosticHead), "�"), strings.ToValidUTF8(string(b.diagnosticTail), "�")
}

func (b *boundedBuffer) evidence() string {
	const evidenceLimit = 2048
	if b.evidenceBytes <= evidenceLimit {
		return string(b.evidenceHead)
	}
	if b.evidenceBytes <= evidenceLimit*2 {
		overlap := evidenceLimit*2 - b.evidenceBytes
		return string(b.evidenceHead) + string(b.evidenceTail[overlap:])
	}
	return string(b.evidenceHead) + "\n...[truncated]...\n" + string(b.evidenceTail)
}

func StderrValue(value []byte) string {
	tail := StderrTail(value)
	if tail == nil {
		return ""
	}
	return *tail
}
