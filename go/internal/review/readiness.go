package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	"purser-hook/internal/proc"
)

type ReadinessRequest struct {
	Grounded           bool   `json:"grounded"`
	Objective          string `json:"objective"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	AttributedDiff     string `json:"attributed_diff"`
	WorkerReport       string `json:"worker_report_untrusted"`
	Agent              string `json:"agent"`
	Model              string `json:"model"`
	RepoRoot           string `json:"repo_root"`
	Executable         string `json:"executable,omitempty"`
}

type ReadinessEvidence struct {
	Path  string `json:"path"`
	Line  int    `json:"line"`
	Quote string `json:"quote"`
}

type ReadinessVerdict struct {
	Verdict   string              `json:"verdict"`
	Criterion *string             `json:"criterion"`
	Evidence  []ReadinessEvidence `json:"evidence"`
}

func unavailableReadiness() ReadinessVerdict {
	return ReadinessVerdict{Verdict: "UNAVAILABLE", Evidence: []ReadinessEvidence{}}
}

func RunReadiness(ctx context.Context, pluginRoot string, request ReadinessRequest, timeout time.Duration) (ReadinessVerdict, error) {
	if timeout <= 0 {
		return unavailableReadiness(), &BridgeError{Code: "INVALID_TIMEOUT"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	python := proc.ResolvePython(ctx, pluginRoot)
	if ctx.Err() != nil {
		return unavailableReadiness(), bridgeContextError(ctx.Err())
	}
	if python == "" {
		return unavailableReadiness(), &BridgeError{Code: "PYTHON_UNAVAILABLE"}
	}
	script := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "readiness_bridge.py")
	deadline, _ := ctx.Deadline()
	return runReadiness(ctx, python, script, request, time.Until(deadline))
}

func runReadiness(parent context.Context, python, script string, request ReadinessRequest, timeout time.Duration) (ReadinessVerdict, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return unavailableReadiness(), &BridgeError{Code: "INVALID_INPUT", Err: err}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, script)
	proc.ConfigureProcessTreeKill(cmd)
	cmd.Stdin = bytes.NewReader(input)
	output, runErr := cmd.Output()
	if ctx.Err() != nil {
		return unavailableReadiness(), bridgeContextError(ctx.Err())
	}
	if runErr != nil {
		if bridgeInvalidInput(output) {
			return unavailableReadiness(), &BridgeError{Code: "INVALID_INPUT", Err: runErr}
		}
		return unavailableReadiness(), &BridgeError{Code: "BRIDGE_PROCESS_FAILED", Err: runErr}
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return unavailableReadiness(), &BridgeError{Code: "BRIDGE_EMPTY_OUTPUT"}
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	var verdict ReadinessVerdict
	if err := decoder.Decode(&verdict); err != nil {
		return unavailableReadiness(), &BridgeError{Code: "BRIDGE_MALFORMED_JSON", Err: err}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return unavailableReadiness(), &BridgeError{Code: "BRIDGE_MALFORMED_JSON", Err: err}
	}
	if !validReadinessVerdict(verdict) {
		return unavailableReadiness(), &BridgeError{Code: "BRIDGE_INVALID_VERDICT"}
	}
	return verdict, nil
}

func validReadinessVerdict(v ReadinessVerdict) bool {
	if v.Evidence == nil {
		return false
	}
	switch v.Verdict {
	case "UNMET":
		if v.Criterion == nil || *v.Criterion == "" || len(v.Evidence) == 0 {
			return false
		}
		for _, citation := range v.Evidence {
			if citation.Path == "" || citation.Line < 1 || citation.Quote == "" {
				return false
			}
		}
		return true
	case "NO_UNMET_FOUND", "UNKNOWN", "UNAVAILABLE":
		return v.Criterion == nil && len(v.Evidence) == 0
	default:
		return false
	}
}
