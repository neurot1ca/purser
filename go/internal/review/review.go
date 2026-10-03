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
)

type Request struct {
	Command         []string
	CWD, Repository string
	Timeout         time.Duration
}

type Outcome struct {
	ReviewStatus string `json:"review_status"`
	Result       string `json:"result"`
	Validation   string `json:"validation"`
	ErrorCode    string `json:"error_code,omitempty"`
}

// Review classifies a reviewer subprocess result.
func Review(request Request) Outcome {
	if request.Timeout <= 0 || request.CWD == "" || request.Repository == "" || len(request.Command) == 0 {
		return unavailable("REVIEWER_RESPONSE_INVALID")
	}
	if unsafe(request.Command, request.Repository) || within(request.CWD, request.Repository) {
		return unavailable("REVIEWER_RESPONSE_INVALID")
	}
	stdout, code := invoke(request.Command, request.CWD, request.Timeout)
	if code != "" {
		return unavailable(code)
	}
	return parse(stdout)
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
	for _, raw := range findings {
		var finding map[string]string
		if json.Unmarshal(raw, &finding) != nil || len(finding) != 2 || finding["code"] == "" || utf8.RuneCountInString(finding["code"]) > 120 || finding["message"] == "" || utf8.RuneCountInString(finding["message"]) > 2000 {
			return invalid()
		}
	}
	if result == "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE" && len(findings) != 0 {
		return invalid()
	}
	return Outcome{ReviewStatus: "COMPLETED", Result: result, Validation: validation}
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
