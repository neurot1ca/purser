package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"purser-hook/internal/models"
)

const pinnedModel = "gemini-3.8-flash-high"

type Request struct {
	Executable, Model, PacketPath, CWD, Repository string
	Schema                                         any
	AddDirs, ExtraFlags                            []string
	Timeout                                        time.Duration
}

type Outcome struct {
	ReviewStatus string    `json:"review_status"`
	Result       string    `json:"result"`
	Validation   string    `json:"validation"`
	Findings     []Finding `json:"findings,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
}

type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type GateDecision struct {
	Run    bool
	Reason string
}

type ModeDecision struct {
	Mode string
	Code string
}

const (
	NoDelta        = "NO_DELTA"
	BelowThreshold = "BELOW_THRESHOLD"
	Audit          = "AUDIT"
	Full           = "FULL"
	PhaseOperation = "PHASE_OPERATION"
	PhasePlan      = "PHASE_PLAN"
)

// Gate applies the mechanical delta threshold before deciding whether to run Reviewer-1.
func Gate(phase, route string, routeKnown bool, deltaFiles, deltaLines, maxFiles, maxLines int, reviewerAvailable bool) GateDecision {
	if phase == "operation" || (phase == "plan" && route == "plan_standard") {
		reason := PhaseOperation
		if phase == "plan" {
			reason = PhasePlan
		}
		return GateDecision{Reason: reason}
	}
	if deltaFiles < 0 || deltaLines < 0 || maxFiles < 0 || maxLines < 0 || phase != "implementation" || !routeKnown {
		return GateDecision{Run: true, Reason: "UNCLASSIFIABLE_INPUT"}
	}
	if deltaFiles == 0 && deltaLines == 0 {
		return GateDecision{Reason: NoDelta}
	}
	if deltaFiles <= maxFiles && deltaLines <= maxLines {
		return GateDecision{Reason: BelowThreshold}
	}
	if !reviewerAvailable {
		return GateDecision{Reason: "REVIEWER_UNAVAILABLE"}
	}
	return GateDecision{Run: true, Reason: "RUN_REVIEWER"}
}

// ReviewMode fails closed to FULL. Skips are never reviewer passes.
func ReviewMode(routeKnown, alwaysFull bool, skipReason, result string) ModeDecision {
	if !routeKnown {
		return ModeDecision{Mode: Full, Code: "UNCLASSIFIABLE_INPUT"}
	}
	if alwaysFull {
		return ModeDecision{Mode: Full, Code: "ALWAYS_FULL_ROUTE"}
	}
	if skipReason != "" {
		if skipReason == NoDelta || skipReason == BelowThreshold {
			return ModeDecision{Mode: Audit, Code: skipReason}
		}
		return ModeDecision{Mode: Full, Code: skipReason}
	}
	switch result {
	case "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE":
		return ModeDecision{Mode: Audit, Code: result}
	case "CHANGES_REQUIRED", "UNCERTAIN", "REVIEWER1_UNAVAILABLE":
		return ModeDecision{Mode: Full, Code: result}
	default:
		return ModeDecision{Mode: Full, Code: "UNCLASSIFIABLE_INPUT"}
	}
}

// SameFamily reports whether a candidate reviewer belongs to the implementer's vendor.
func SameFamily(implementer, reviewer string) bool {
	return vendorFamily(implementer) != "" && vendorFamily(implementer) == vendorFamily(reviewer)
}

func vendorFamily(family string) string {
	family = strings.ToLower(strings.TrimSpace(family))
	if i := strings.IndexAny(family, "-/"); i >= 0 {
		family = family[:i]
	}
	switch family {
	case "gpt", "openai":
		return "openai"
	case "gemini", "google":
		return "google"
	case "claude", "anthropic":
		return "anthropic"
	case "qwen3":
		return "qwen"
	case "llama3":
		return "llama"
	default:
		for _, known := range []string{"qwen", "mistral", "llama"} {
			if strings.HasPrefix(family, known) && strings.Trim(family[len(known):], "0123456789") == "" {
				return known
			}
		}
		return family
	}
}

// Passed is true only for an executed, clean reviewer result with passing validation.
func Passed(outcome Outcome, skippedReason string) bool {
	return skippedReason == "" && outcome.ReviewStatus == "COMPLETED" && outcome.Validation == "PASS" && outcome.Result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE"
}

// Review classifies a reviewer subprocess result.
func Review(request Request) Outcome {
	if request.Timeout <= 0 || request.CWD == "" || request.Repository == "" || request.PacketPath == "" {
		return unavailable("REVIEWER_RESPONSE_INVALID")
	}
	command, err := buildCommand(request)
	if err != nil {
		return unavailable("REVIEWER_RESPONSE_INVALID")
	}
	cwd, err := resolve(request.CWD)
	if err != nil || within(cwd, request.Repository) {
		return unavailable("REVIEWER_RESPONSE_INVALID")
	}
	stdout, code := invoke(command, cwd, request.Timeout)
	if code != "" {
		return unavailable(code)
	}
	return parse(stdout)
}

func buildCommand(request Request) ([]string, error) {
	if err := (reviewerAdapter{"agy"}).ValidateModel(request.Model); err != nil {
		return nil, err
	}
	request.Model = models.ResolveLatest("agy", request.Model)
	executable := request.Executable
	if !filepath.IsAbs(executable) {
		var err error
		executable, err = exec.LookPath("agy")
		if err != nil {
			return nil, err
		}
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		} else {
			executable, err = filepath.Abs(executable)
			if err != nil {
				return nil, err
			}
		}
	}
	schema, err := canonicalJSON(request.Schema)
	if err != nil {
		return nil, err
	}
	command := []string{executable, "--print", reviewPrompt(request.PacketPath), "--output-format", "json", "--json-schema", string(schema), "--model", request.Model, "--mode", "plan", "--sandbox"}
	for _, dir := range request.AddDirs {
		path, err := resolve(dir)
		if err != nil {
			return nil, err
		}
		command = append(command, "--add-dir", path)
	}
	command = append(command, request.ExtraFlags...)
	if unsafe(command, request.Repository) {
		return nil, errors.New("unsafe reviewer command")
	}
	return command, nil
}

func reviewPrompt(path string) string {
	return "You are Reviewer-1. Read and review only the JSON packet at the exact path below. Do not inspect unrelated files, infer hidden reasoning, or accept an implementer narrative. Return only the supplied JSON schema. Use NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE only when the complete diff and supplied contract establish no material issue. Use UNCERTAIN when the diff or context needed to assess the diff is insufficient. The normal absence of orchestrator validation evidence is not by itself grounds for UNCERTAIN; report validation UNKNOWN without inferring that validation ran.\n\nREVIEW_PACKET_PATH=" + path
}

func canonicalJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte("\n")), nil
}

func unsafe(command []string, repository string) bool {
	plan, disabled := false, false
	for i, arg := range command {
		if arg == "--mode" && i+1 < len(command) && command[i+1] == "plan" {
			plan = true
		}
		if arg == "--disable-slash-commands" {
			disabled = true
		}
	}
	if plan && disabled {
		return true
	}
	for i, arg := range command {
		if arg != "--add-dir" || i+1 >= len(command) {
			continue
		}
		if repository == "" || within(command[i+1], repository) {
			return true
		}
	}
	return false
}

func invoke(command []string, cwd string, timeout time.Duration) ([]byte, string) {
	started, remaining, retried := time.Now(), timeout, false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Dir = cwd
		cmd.Env = os.Environ()
		cmd.Stdin = os.Stdin
		configureLaunch(cmd)
		stdout, err := cmd.Output()
		elapsed, deadline := time.Since(started), errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			return stdout, ""
		}
		if deadline {
			return nil, "REVIEWER_TIMEOUT"
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return nil, "REVIEWER_EXECUTABLE_MISSING"
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, "REVIEWER_RESPONSE_INVALID"
		}
		left := timeout - elapsed
		if !retried && elapsed < 60*time.Second && left > 5*time.Second {
			retried, remaining = true, left
			continue
		}
		return nil, "REVIEWER_PROCESS_FAILED"
	}
}

func parse(stdout []byte) Outcome {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(stdout, &envelope) != nil {
		return invalid()
	}
	for _, key := range []string{"conversation_id", "status", "response", "duration_seconds", "num_turns", "usage"} {
		if _, ok := envelope[key]; !ok {
			return invalid()
		}
	}
	var conversation, status string
	var duration float64
	var turns int
	var usage map[string]json.RawMessage
	if json.Unmarshal(envelope["conversation_id"], &conversation) != nil || conversation == "" || json.Unmarshal(envelope["status"], &status) != nil || !strings.EqualFold(status, "success") || json.Unmarshal(envelope["duration_seconds"], &duration) != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0 || json.Unmarshal(envelope["num_turns"], &turns) != nil || turns < 1 || json.Unmarshal(envelope["usage"], &usage) != nil || usage == nil {
		return invalid()
	}
	payload := envelope["structured_output"]
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		payload = envelope["response"]
	}
	var encoded string
	if json.Unmarshal(payload, &encoded) == nil {
		payload = []byte(encoded)
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(payload, &response) != nil || len(response) != 3 {
		return invalid()
	}
	var result, validation string
	var findings []json.RawMessage
	if json.Unmarshal(response["result"], &result) != nil || json.Unmarshal(response["validation"], &validation) != nil || json.Unmarshal(response["findings"], &findings) != nil || findings == nil || len(findings) > 20 || (validation != "PASS" && validation != "FAIL" && validation != "UNKNOWN") {
		return invalid()
	}
	if result == "REVIEWER1_UNAVAILABLE" {
		return unavailable("REVIEWER_MODEL_REPORTED_UNAVAILABLE")
	}
	if result != "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" && result != "CHANGES_REQUIRED" && result != "UNCERTAIN" {
		return invalid()
	}
	validatedFindings := make([]Finding, 0, len(findings))
	for _, raw := range findings {
		var finding map[string]string
		if json.Unmarshal(raw, &finding) != nil || len(finding) != 2 || finding["code"] == "" || utf8.RuneCountInString(finding["code"]) > 120 || finding["message"] == "" || utf8.RuneCountInString(finding["message"]) > 2000 {
			return invalid()
		}
		validatedFindings = append(validatedFindings, Finding{Code: finding["code"], Message: finding["message"]})
	}
	if result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" && len(findings) != 0 {
		return invalid()
	}
	if result == "CHANGES_REQUIRED" && len(findings) == 0 {
		return invalid()
	}
	return Outcome{ReviewStatus: "COMPLETED", Result: result, Validation: validation, Findings: validatedFindings}
}

func unavailable(code string) Outcome {
	return Outcome{ReviewStatus: "UNAVAILABLE", Result: "REVIEWER1_UNAVAILABLE", Validation: "UNKNOWN", ErrorCode: code}
}
func invalid() Outcome { return unavailable("REVIEWER_RESPONSE_INVALID") }

func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func within(path, root string) bool {
	p, e1 := resolve(path)
	r, e2 := resolve(root)
	if e1 != nil || e2 != nil {
		return true
	}
	rel, err := filepath.Rel(r, p)
	return err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
