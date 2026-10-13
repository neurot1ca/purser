package maintenance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/models"
	"purser-hook/internal/obsidian"
	"purser-hook/internal/policy"
)

const (
	JobTimeout   = 45 * time.Minute
	PollInterval = time.Second
)

// JobOperations is the small boundary around the Go MCP host's jobs operations.
// Implementations call the existing jobs start/result/cancel operations.
type JobOperations interface {
	Start(state string, args map[string]any) (map[string]any, error)
	Result(state, jobID string) map[string]any
	Cancel(state, jobID string) map[string]any
}

type Runner struct {
	Jobs         JobOperations
	CoderAgent   string
	CoderModel   string
	CoderEffort  string
	Now          func() time.Time
	Sleep        func(time.Duration)
	PollInterval time.Duration
	JobTimeout   time.Duration
}

// Run starts a read-only maintenance job and returns its raw terminal result.
// Prompt construction, result parsing, persistence, and telemetry belong to the
// caller so the lifecycle can be shared by both maintenance report types.
func (r Runner) Run(state, repository, reportType, prompt string) (map[string]any, error) {
	if r.Jobs == nil {
		return nil, errors.New("maintenance job operations are required")
	}
	if reportType != "cleanup" && reportType != "test-gaps" {
		return nil, fmt.Errorf("unsupported maintenance report type: %s", reportType)
	}
	if err := r.resolveCoder(repository); err != nil {
		return nil, err
	}
	now, sleep, poll, timeout := r.clock()
	root, err := filepath.Abs(repository)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	maintenanceDir := filepath.Join(state, "projects", contextio.ProjectKey(root), "maintenance")
	activePath := filepath.Join(maintenanceDir, reportType+"-active.json")
	if err := os.MkdirAll(maintenanceDir, 0700); err != nil {
		return nil, err
	}
	releaseSlot, err := contextio.AcquireMaintenanceSlot(state)
	if err != nil {
		return nil, err
	}
	retainSlot := false
	defer func() {
		if !retainSlot {
			releaseSlot()
		}
	}()
	startedAt := now().UTC().Format("20060102T150405.000000Z")
	active := map[string]any{"project_root": root, "started_at": startedAt, "state": "RUNNING"}
	if err := contextio.CreateOrRecoverMaintenanceActive(activePath, state, active, now()); err != nil {
		return nil, err
	}
	defer func() { _ = contextio.ClearMaintenanceActive(activePath) }()
	if userJobs, known := activeUserJobs(state, ""); !known || userJobs {
		return nil, errors.New("active or unknown user job state; maintenance skipped")
	}
	call, err := randomID()
	if err != nil {
		return nil, err
	}
	started, err := r.Jobs.Start(state, map[string]any{
		"call": call, "mode": "new", "prompt": prompt, "cwd": root,
		"sandbox": "read-only", "jobPhase": "operation", "agent": r.CoderAgent,
		"jobModel": r.CoderModel, "jobEffort": r.CoderEffort,
		"timeoutSeconds": int(timeout.Seconds()),
	})
	if err != nil {
		return nil, err
	}
	started = structured(started)
	jobID, ok := started["jobId"].(string)
	if !ok || jobID == "" {
		return nil, errors.New(stringValue(started["message"], "durable "+coderLabel(r.CoderAgent)+" job could not be started"))
	}
	active["job_id"] = jobID
	if err := replaceActive(activePath, active); err != nil {
		return nil, err
	}
	for {
		if userJobs, known := activeUserJobs(state, jobID); known && userJobs {
			cancelled := structured(r.Jobs.Cancel(state, jobID))["state"] == "CANCELLED"
			if !cancelled {
				cancelDeadline := now().Add(timeout)
				for now().Before(cancelDeadline) {
					result := structured(r.Jobs.Result(state, jobID))
					if terminal(result["state"], false) {
						return nil, fmt.Errorf("maintenance yielded to a user %s job", coderLabel(r.CoderAgent))
					}
					sleep(minDuration(poll, cancelDeadline.Sub(now())))
				}
				retainSlot = true
				return nil, errors.New("maintenance cancellation was unconfirmed; global slot retained by job timeout")
			}
			return nil, fmt.Errorf("maintenance yielded to a user %s job", coderLabel(r.CoderAgent))
		}
		result := structured(r.Jobs.Result(state, jobID))
		status := stringValue(result["state"], "")
		if status == "SUCCEEDED" {
			if result["jobId"] == nil {
				result["jobId"] = jobID
			}
			return result, nil
		}
		if terminal(status, true) {
			if result["jobId"] == nil {
				result["jobId"] = jobID
			}
			return result, fmt.Errorf("%s job %s: %s %s", coderLabel(r.CoderAgent), status, stringValue(result["errorCode"], ""), stringValue(result["message"], ""))
		}
		sleep(poll)
	}
}

func coderLabel(agent string) string {
	switch agent {
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "agy":
		return "Antigravity"
	case "claude":
		return "Claude"
	default:
		return agent
	}
}

func (r *Runner) resolveCoder(repository string) error {
	if r.CoderAgent != "" && r.CoderModel != "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	config, err := policy.LoadCoderConfigForRepository(obsidian.ConfigPath(), filepath.Join(home, ".orchestrator", "config.toml"), repository)
	if err != nil {
		return fmt.Errorf("maintenance coder configuration: %w", err)
	}
	agent, _ := config["coder_agent"].(string)
	routes, _ := config["routes"].(map[string]any)
	route, _ := routes["operation"].(map[string]any)
	model, _ := route["model"].(string)
	effort, _ := route["effort"].(string)
	if agent == "" || model == "" {
		return errors.New("maintenance operation coder route is unavailable")
	}
	r.CoderAgent, r.CoderModel, r.CoderEffort = agent, models.ResolveLatest(agent, model), effort
	return nil
}

func (r Runner) clock() (func() time.Time, func(time.Duration), time.Duration, time.Duration) {
	now, sleep, poll, timeout := r.Now, r.Sleep, r.PollInterval, r.JobTimeout
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	if poll <= 0 {
		poll = PollInterval
	}
	if timeout <= 0 {
		timeout = JobTimeout
	}
	return now, sleep, poll, timeout
}

func activeUserJobs(state, exclude string) (bool, bool) {
	dir := filepath.Join(state, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return false, false
		}
		var job map[string]any
		if json.Unmarshal(data, &job) != nil {
			return false, false
		}
		status, ok := job["state"].(string)
		if !ok {
			return false, false
		}
		idValue, exists := job["jobId"]
		id, ok := idValue.(string)
		if !exists {
			id = strings.TrimSuffix(entry.Name(), ".json")
		} else if !ok {
			id = ""
		}
		if id != exclude && (status == "RUNNING" || status == "QUEUED") {
			return true, true
		}
	}
	return false, true
}

func replaceActive(path string, active map[string]any) error {
	data, err := json.Marshal(active)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".maintenance-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(id[0:4]), hex.EncodeToString(id[4:6]), hex.EncodeToString(id[6:8]), hex.EncodeToString(id[8:10]), hex.EncodeToString(id[10:])), nil
}

func structured(value map[string]any) map[string]any {
	if nested, ok := value["structuredContent"].(map[string]any); ok {
		return nested
	}
	return value
}

func terminal(value any, includeUnknown bool) bool {
	switch value {
	case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT":
		return true
	case "UNKNOWN_OR_IN_PROGRESS":
		return includeUnknown
	default:
		return false
	}
}

func stringValue(value any, fallback string) string {
	if s, ok := value.(string); ok {
		return s
	}
	return fallback
}
func minDuration(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}
