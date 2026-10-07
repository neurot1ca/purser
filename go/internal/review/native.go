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
		return BridgeOutcome{"completed", result.Map()}, nil
	case UnavailableReview:
		return BridgeOutcome{"unavailable", result.Map()}, nil
	case IncompleteReview:
		return BridgeOutcome{"incomplete", result.Map()}, nil
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
	return BridgeOutcome{"unavailable", attrs}, &BridgeError{Code: "REVIEWER_BRIDGE_EXCEPTION", Err: fmt.Errorf("reviewer panic (%s)", name)}
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
	return reviewMaterials(ctx, materials, config, kwargs)
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
	working, err := os.MkdirTemp("", "reviewer1-")
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
	command, err := adapter.BuildCommand(ReviewPrompt(packetPath), reviewSchema, config, options)
	if err != nil {
		code := ResponseInvalid
		var refused *CommandRefused
		if errors.As(err, &refused) {
			code = "REVIEWER_COMMAND_REFUSED"
		}
		return bridgeOutcome(Unavailable(code, "Reviewer-1 response or configuration was invalid ("+errorType(err)+"): "+err.Error(), config, provider))
	}
	stdout, stderr, elapsed, retries, callErr, exitCode := invokePacket(ctx, adapter, command, working, remaining)
	if callErr != "" {
		unavailable := Unavailable(callErr, reviewerFailureMessage(callErr, retries, exitCode), config, provider)
		unavailable.ElapsedSeconds = nativeFloatPtr(elapsed.Seconds())
		if stderr != "" {
			unavailable.StderrTail = StderrTail(stderr)
		}
		return bridgeOutcome(unavailable)
	}
	parsed, err := adapter.ParseEnvelope(string(stdout))
	if err != nil {
		unavailable := Unavailable(ResponseInvalid, fmt.Sprintf("Reviewer-1 response or configuration was invalid (%s): %s", errorType(err), err.Error())+retrySuffix(retries), config, provider)
		unavailable.RawStdout = textPtr(lastBytes(stdout, 262144))
		detail := err.Error()
		if len(detail) > 200 {
			detail = detail[:200]
		}
		unavailable.DecoderDetail = &detail
		unavailable.ElapsedSeconds = nativeFloatPtr(elapsed.Seconds())
		return bridgeOutcome(unavailable)
	}
	response := parsed["response"].(map[string]any)
	if response["result"] == Reviewer1Unavailable {
		unavailable := Unavailable("REVIEWER_MODEL_REPORTED_UNAVAILABLE", "Reviewer-1 returned REVIEWER1_UNAVAILABLE."+retrySuffix(retries), config, provider)
		unavailable.ElapsedSeconds = nativeFloatPtr(elapsed.Seconds())
		if stderr != "" {
			unavailable.StderrTail = StderrTail(stderr)
		}
		return bridgeOutcome(unavailable)
	}
	result, _ := response["result"].(string)
	findings, _ := response["findings"].([]any)
	return bridgeOutcome(CompletedReview{Result: result, Validation: response["validation"].(string), Findings: findingsFromAny(findings), ConversationID: parsed["conversation_id"].(string), Reviewer1ModelRequested: RequestedModel(config), DurationSeconds: parsed["duration_seconds"].(*float64), Usage: parsed["usage"].(map[string]any), Provider: provider, ServedModelAttestation: fmt.Sprint(parsed["served_model_attestation"]), RetryCount: retries, RetryRecovered: retries > 0})
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

func invokePacket(parent context.Context, adapter Adapter, command []string, cwd string, timeout time.Duration) ([]byte, string, time.Duration, int, string, int) {
	started := time.Now()
	retried := 0
	for {
		remaining := timeout - time.Since(started)
		if remaining <= 0 {
			return nil, "", time.Since(started), retried, "REVIEWER_TIMEOUT", 0
		}
		ctx, cancel := context.WithTimeout(parent, remaining)
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Dir = cwd
		cmd.Env = os.Environ()
		for key, value := range adapter.Environment(command) {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		proc.ConfigureProcessTreeKill(cmd)
		configureNativeLaunch(cmd, adapter.LaunchOptions())
		var stdout, stderr boundedBuffer
		stdout.limit = maxReviewerStdoutBytes
		stderr.limit, stderr.tail = maxReviewerStderrBytes, true
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		elapsed := time.Since(started)
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			if stdout.overflowed {
				return nil, StderrValue(stderr.Bytes()), elapsed, retried, ResponseInvalid, 0
			}
			return stdout.Bytes(), StderrValue(stderr.Bytes()), elapsed, retried, "", 0
		}
		if timedOut {
			return nil, StderrValue(stderr.Bytes()), elapsed, retried, "REVIEWER_TIMEOUT", 0
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return nil, StderrValue(stderr.Bytes()), elapsed, retried, "REVIEWER_EXECUTABLE_MISSING", 0
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, StderrValue(stderr.Bytes()), elapsed, retried, ResponseInvalid, 0
		}
		left := timeout - elapsed
		if retried == 0 && elapsed < time.Duration(fastProcessFailureRetrySeconds*float64(time.Second)) && left > time.Duration(minRetryRemainingSeconds*float64(time.Second)) {
			retried = 1
			continue
		}
		return nil, StderrValue(stderr.Bytes()), elapsed, retried, "REVIEWER_PROCESS_FAILED", exit.ExitCode()
	}
}

type boundedBuffer struct {
	bytes.Buffer
	limit      int
	tail       bool
	overflowed bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
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

func StderrValue(value []byte) string {
	tail := StderrTail(value)
	if tail == nil {
		return ""
	}
	return *tail
}
