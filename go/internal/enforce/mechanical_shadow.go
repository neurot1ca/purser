package enforce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

const mechanicalShadowTimeout = 45 * time.Second
const mechanicalShadowLockMaxAge = mechanicalShadowTimeout + 600*time.Second

// RunMechanicalShadowJob executes one detached mechanical shadow job.
func RunMechanicalShadowJob(jobPath string) bool {
	return runMechanicalShadowJob(context.Background(), jobPath, "", mechanicalShadowTimeout)
}

type mechanicalShadowRecord struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Repository     string `json:"repository"`
	Scratch        string `json:"scratch"`
	AcceptedReport string `json:"accepted_report"`
	ProjectKey     string `json:"project_key"`
	StateRoot      string `json:"state_root"`
}

type mechanicalShadowJob struct {
	Model      string
	Prompt     string
	Repository string
	Scratch    string
}

type mechanicalShadowOutcome struct {
	Stdout   string
	ExitCode int
	TimedOut bool
}

var mechanicalShadowWord = regexp.MustCompile(`[a-z0-9]+`)

func runMechanicalShadowJob(ctx context.Context, jobPath, executable string, timeout time.Duration) bool {
	var job mechanicalShadowRecord
	data, err := os.ReadFile(jobPath)
	if err != nil || json.Unmarshal(data, &job) != nil {
		return false
	}
	started := time.Now()
	lock := filepath.Join(filepath.Dir(jobPath), "running.lock")
	_ = os.WriteFile(lock, []byte(strconv.Itoa(os.Getpid())), 0600)
	outcome, err := runMechanicalShadow(ctx, mechanicalShadowJob{Model: job.Model, Prompt: job.Prompt, Repository: job.Repository, Scratch: job.Scratch}, executable, timeout)
	tokens := mechanicalShadowTokens(outcome.Stdout)
	completed, success := false, false
	if err == nil && outcome.ExitCode == 0 && !outcome.TimedOut {
		if report, e := mechanicalShadowReport(outcome.Stdout); e == nil {
			completed, success = true, mechanicalReportsAgree(report, job.AcceptedReport)
		}
	}
	positions, sampleErr := mechanicalShadowRecordSample(job.StateRoot, job.Model, success)
	if sampleErr == nil && positions[0] >= 0 && positions[1] >= 0 && positions[0] != positions[1] {
		preEmit(job.StateRoot, map[string]any{}, nil, "shadow-job", job.ProjectKey, "MECHANICAL_MODEL_RANK_CHANGED", map[string]any{"served_model": job.Model, "old_rank": positions[0] + 1, "new_rank": positions[1] + 1})
	}
	payload := map[string]any{"candidate": job.Model, "status": "failed", "elapsed_ms": float64(time.Since(started).Microseconds()) / 1000}
	if completed {
		payload["status"], payload["agreement"] = "completed", success
	}
	for key, value := range tokens {
		payload[key] = value
	}
	preEmit(job.StateRoot, map[string]any{}, nil, "shadow-job", job.ProjectKey, "SHADOW_FINISHED", payload)
	_ = os.RemoveAll(job.Scratch)
	_ = os.Remove(jobPath)
	_ = os.Remove(lock)
	return success
}

func mechanicalShadowRecordSample(root, model string, success bool) ([2]int, error) {
	rank := func() (int, error) {
		data, err := os.ReadFile(filepath.Join(root, "opencode", "candidates.json"))
		if err != nil {
			return -1, err
		}
		var value struct {
			Candidates []map[string]any `json:"candidates"`
		}
		if err = json.Unmarshal(data, &value); err != nil {
			return -1, err
		}
		for i, candidate := range value.Candidates {
			if candidate["id"] == model {
				return i, nil
			}
		}
		return -1, nil
	}
	before, err := rank()
	if err != nil {
		return [2]int{-1, -1}, err
	}
	if err = recordMechanicalSample(root, model, success); err != nil {
		return [2]int{-1, -1}, err
	}
	after, err := rank()
	return [2]int{before, after}, err
}

func mechanicalShadowReport(stdout string) (string, error) {
	var texts []string
	completed := false
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return "", err
		}
		if event["type"] == "error" {
			return "", fmt.Errorf("OpenCode reported an error")
		}
		part, _ := event["part"].(map[string]any)
		if event["type"] == "text" && part != nil {
			if text, ok := part["text"].(string); ok {
				texts = append(texts, text)
			}
		}
		if event["type"] == "step_finish" && part != nil && part["reason"] == "stop" {
			completed = true
		}
	}
	report := strings.TrimSpace(strings.Join(texts, ""))
	if !completed || report == "" {
		return "", fmt.Errorf("OpenCode returned no completed report")
	}
	return report, nil
}

func mechanicalReportsAgree(candidate, accepted string) bool {
	left, right := map[string]bool{}, map[string]bool{}
	for _, word := range mechanicalShadowWord.FindAllString(strings.ToLower(candidate), -1) {
		left[word] = true
	}
	for _, word := range mechanicalShadowWord.FindAllString(strings.ToLower(accepted), -1) {
		right[word] = true
	}
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	intersection, union := 0, 0
	for word := range left {
		union++
		if right[word] {
			intersection++
		}
		if _, err := strconv.Atoi(word); err == nil && !right[word] {
			return false
		}
	}
	for word := range right {
		if !left[word] {
			union++
		}
		if _, err := strconv.Atoi(word); err == nil && !left[word] {
			return false
		}
	}
	return float64(intersection)/float64(union) >= .5
}

func mechanicalShadowTokens(stdout string) map[string]any {
	totals := map[string]any{}
	keys := map[string][2]string{"input_tokens": {"input", "input_tokens"}, "output_tokens": {"output", "output_tokens"}, "total_tokens": {"total", "total_tokens"}}
	for _, line := range strings.Split(stdout, "\n") {
		var event struct {
			Type string `json:"type"`
			Part struct {
				Tokens map[string]json.RawMessage `json:"tokens"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Type != "step_finish" {
			continue
		}
		for target, sources := range keys {
			for _, source := range sources {
				raw, ok := event.Part.Tokens[source]
				if !ok {
					continue
				}
				var value int
				if json.Unmarshal(raw, &value) != nil || value < 0 {
					break
				}
				previous, _ := totals[target].(int)
				totals[target] = previous + value
				break
			}
		}
	}
	return totals
}

func recoverStaleMechanicalShadow(shadows string, now time.Time) (bool, error) {
	lock := filepath.Join(shadows, "running.lock")
	info, err := os.Stat(lock)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	stale := now.Sub(info.ModTime()) > mechanicalShadowLockMaxAge
	if !stale {
		return false, nil
	}
	for _, pattern := range []string{"*.json", "run-*"} {
		matches, _ := filepath.Glob(filepath.Join(shadows, pattern))
		for _, path := range matches {
			_ = os.RemoveAll(path)
		}
	}
	return true, os.Remove(lock)
}

func init() {
	observationFields["MECHANICAL_MODEL_RANK_CHANGED"] = fields("served_model old_rank new_rank")
	observationFields["SHADOW_FINISHED"] = fields("candidate status elapsed_ms input_tokens output_tokens total_tokens agreement")
	setObservationRule("id", "served_model candidate")
	setObservationRule("int", "old_rank new_rank input_tokens output_tokens total_tokens")
	setObservationRule("number", "elapsed_ms")
	setObservationRule("enum:completed failed", "status")
	setObservationRule("bool", "agreement")
}

func runMechanicalShadow(ctx context.Context, job mechanicalShadowJob, executable string, timeout time.Duration) (mechanicalShadowOutcome, error) {
	var outcome mechanicalShadowOutcome
	source, err := filepath.Abs(job.Repository)
	if err != nil {
		return outcome, err
	}
	scratch, err := filepath.Abs(job.Scratch)
	if err != nil {
		return outcome, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return outcome, err
	}
	if pathsOverlap(source, scratch) {
		return outcome, fmt.Errorf("mechanical shadow scratch overlaps repository")
	}
	scratch, err = resolveMechanicalPath(scratch)
	if err != nil {
		return outcome, err
	}
	if pathsOverlap(source, scratch) {
		return outcome, fmt.Errorf("mechanical shadow scratch overlaps repository")
	}
	if executable == "" {
		executable, err = exec.LookPath("opencode")
		if err != nil {
			return outcome, err
		}
	}
	if timeout <= 0 {
		timeout = mechanicalShadowTimeout
	}
	if err = os.MkdirAll(filepath.Dir(scratch), 0700); err != nil {
		return outcome, err
	}
	if err = os.Mkdir(scratch, 0700); err != nil {
		return outcome, err
	}
	defer os.RemoveAll(scratch)
	if err = copyMechanicalShadow(source, scratch); err != nil {
		return outcome, err
	}
	permissions := map[string]string{"edit": "ask", "write": "ask", "bash": "ask", "webfetch": "ask", "task": "ask", "external_directory": "ask"}
	config, err := json.Marshal(map[string]any{
		"permission": permissions,
		"agent":      map[string]any{"purser-shadow": map[string]any{"mode": "primary", "permission": permissions}},
	})
	if err != nil {
		return outcome, err
	}
	argv := []string{"run", "--format", "json", "--model", job.Model, "--agent", "purser-shadow", "--dir", scratch, job.Prompt}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, executable, argv...)
	cmd.Dir = scratch
	cmd.Env = append(os.Environ(), "OPENCODE_CONFIG_CONTENT="+string(config))
	cmd.Stdin, cmd.Stderr = nil, io.Discard
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	proc.ConfigureProcessTreeKill(cmd)
	err = cmd.Run()
	outcome.Stdout = stdout.String()
	outcome.TimedOut = commandCtx.Err() == context.DeadlineExceeded
	if cmd.ProcessState != nil {
		outcome.ExitCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		return outcome, err
	}
	return outcome, nil
}

func copyMechanicalShadow(source, scratch string) error {
	ignored := map[string]bool{".git": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ignored[entry.Name()] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(scratch, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Mkdir(destination, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported mechanical shadow input: %s", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, info.Mode().Perm())
	})
}

func pathsOverlap(a, b string) bool {
	inside := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	return inside(a, b) || inside(b, a)
}

func resolveMechanicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := filepath.Dir(abs)
	for {
		if _, err = os.Stat(ancestor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", err
		}
		ancestor = parent
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(ancestor, abs)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, rel), nil
}
