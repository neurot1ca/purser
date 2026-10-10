package enforce

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

const postRepairOutputLimit = 1 << 20

type postRepairFinding struct{ Code, Message string }
type postRepairProcess struct {
	stdout, stderr string
	err            error
	timedOut       bool
}
type postRepairRunner func(context.Context, []string, []string, string, string, time.Duration) postRepairProcess

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := b.limit - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

// attemptPostMicroRepair mirrors the bounded subprocess portion of codex.py's
// post_tool_use repair branch. It intentionally stops before delta/re-review.
func attemptPostMicroRepair(bundle map[string]any, agent, result, threadID, repoRoot string,
	findings []postRepairFinding, scope []string, enabled bool, configuredTimeout int, availableBudget time.Duration, runner postRepairRunner) (map[string]any, bool) {
	if agent != "codex" || !enabled || result != "CHANGES_REQUIRED" || threadID == "" {
		return nil, false
	}
	initial := findings
	if len(initial) > 5 {
		initial = initial[:5]
	}
	initialAny := make([]any, 0, len(initial))
	for _, finding := range initial {
		initialAny = append(initialAny, map[string]any{"code": finding.Code, "message": finding.Message})
	}
	if availableBudget < 15*time.Second {
		bundle["micro_repair"] = map[string]any{"attempted": false, "status": "SKIPPED_BUDGET", "initial_findings": initialAny}
		return nil, true
	}
	timeout := time.Duration(configuredTimeout) * time.Second
	if budget := time.Duration(int(availableBudget.Seconds())) * time.Second; budget < timeout {
		timeout = budget
	}
	entry := map[string]any{"attempted": true, "status": "RUNNING", "initial_findings": initialAny}
	bundle["micro_repair"], bundle["micro_repair_cleared_review"] = entry, false
	prompt := postRepairPrompt(findings, scope)
	if runner == nil {
		runner = runPostRepairProcess
	}
	process := runner(context.Background(), []string{"codex", "exec", "resume", threadID, "-", "--sandbox", "workspace-write"}, os.Environ(), repoRoot, prompt, timeout)
	switch {
	case process.err == nil && !process.timedOut:
		return map[string]any{"repaired": true, "output": process.stdout}, true
	case process.timedOut:
		entry["status"] = "TIMED_OUT"
		return map[string]any{"repaired": false, "error": fmt.Sprintf("TimeoutExpired: %ds", int(timeout.Seconds()))}, true
	default:
		entry["status"] = "FAILED"
		detail := process.stderr
		if detail == "" {
			detail = process.stdout
		}
		if detail == "" && process.err != nil {
			detail = process.err.Error()
		}
		return map[string]any{"repaired": false, "error": detail}, true
	}
}

func runPostRepairProcess(parent context.Context, argv []string, env []string, cwd, prompt string, timeout time.Duration) postRepairProcess {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := newPostRepairCommand(ctx, argv...)
	cmd.Env, cmd.Dir, cmd.Stdin = env, cwd, strings.NewReader(prompt)
	stdout, stderr := &cappedBuffer{limit: postRepairOutputLimit}, &cappedBuffer{limit: postRepairOutputLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	return postRepairProcess{stdout: stdout.String(), stderr: stderr.String(), err: err, timedOut: ctx.Err() == context.DeadlineExceeded}
}

func newPostRepairCommand(ctx context.Context, argv ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.ConfigureProcessTreeKill(cmd)
	return proc.Hide(cmd)
}

func postRepairPrompt(findings []postRepairFinding, scope []string) string {
	var bullets []string
	for _, finding := range findings {
		if finding.Code != "" {
			bullets = append(bullets, "- "+finding.Code+": "+finding.Message)
		}
	}
	bulletText := strings.Join(bullets, "\n")
	if runes := []rune(bulletText); len(runes) > 500 {
		bulletText = string(runes[:500])
	}
	scopeText := ""
	if len(scope) > 0 {
		scopeText = "\nYou are strictly restricted to modifying files within scope: " + strings.Join(scope, ", ") + "."
	}
	return "Micro-repair request: Reviewer-1 flagged the following issues in your previous diff:\n" + bulletText + scopeText + "\nPlease fix these issues directly in the worktree and ensure tests pass."
}
