package review

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
)

type CheckResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	ExitCode   *int   `json:"exit_code"`
	DurationMS int    `json:"duration_ms"`
	Tail       string `json:"tail"`
}

// SelectChecks applies the profile's protected-path glob semantics.
func SelectChecks(checks []profile.Check, changedPaths []string) []profile.Check {
	selected := make([]profile.Check, 0, len(checks))
	for _, check := range checks {
		if check.When == nil || anyProtected(check.When, changedPaths) {
			selected = append(selected, check)
		}
	}
	return selected
}

func anyProtected(patterns, paths []string) bool {
	for _, path := range paths {
		if profile.IsProtected(path, patterns) {
			return true
		}
	}
	return false
}

// RunChecks uses one context deadline for the ordered check set. The context also
// carries caller cancellation; each check's timeout can shorten that deadline.
func RunChecks(repoRoot string, checks []profile.Check, deadline context.Context) []CheckResult {
	results := make([]CheckResult, 0, len(checks))
	for _, check := range checks {
		if deadline.Err() != nil {
			results = append(results, CheckResult{Name: check.Name, Status: "UNKNOWN", Tail: "not run: deadline"})
			continue
		}
		results = append(results, runCheck(repoRoot, check, deadline))
	}
	return results
}

func runCheck(repoRoot string, check profile.Check, deadline context.Context) CheckResult {
	started := time.Now()
	result := CheckResult{Name: check.Name}
	executable, err := exec.LookPath(check.Argv[0])
	if err != nil {
		result.Status, result.Tail = "UNKNOWN", tail("unresolvable executable: "+check.Argv[0])
		return result
	}
	ctx, cancel := context.WithTimeout(deadline, time.Duration(check.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, check.Argv[1:]...)
	cmd.Dir = repoRoot
	for _, value := range cmd.Environ() {
		if !strings.HasPrefix(value, "CI=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "CI=1")
	proc.ConfigureProcessTreeKill(cmd)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	result.DurationMS = int(time.Since(started).Milliseconds())
	switch {
	case ctx.Err() != nil:
		result.Status, result.Tail = "UNKNOWN", tail(output.String()+"\ntimed out")
	case err == nil:
		code := 0
		result.Status, result.ExitCode = "PASS", &code
	case cmd.ProcessState != nil:
		code := cmd.ProcessState.ExitCode()
		result.Status, result.ExitCode, result.Tail = "FAIL", &code, tail(output.String())
	default:
		result.Status, result.Tail = "UNKNOWN", tail("launch error: "+err.Error())
	}
	return result
}

func tail(output string) string {
	output = strings.ToValidUTF8(output, "\uFFFD")
	runes := []rune(output)
	if len(runes) > 1500 {
		runes = runes[len(runes)-1500:]
	}
	return string(runes)
}

// CompareCheckMutations is the pass-91 seam; mutations are not compared yet.
func CompareCheckMutations(string, string) string { return "not yet compared" }

// AggregateCheckStatus is the pass-91 seam; aggregate status is not compared yet.
func AggregateCheckStatus([]CheckResult) string { return "not yet compared" }
