package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"purser-hook/internal/enforce"
	"purser-hook/internal/ledger"
	"purser-hook/internal/obsidian"
	"purser-hook/internal/policy"
	"purser-hook/internal/review"
)

func checkReadinessBeforeSuccess(dir, jobID string, result map[string]any) {
	started := time.Now()
	verdict := review.ReadinessVerdict{Verdict: "UNKNOWN", Evidence: []review.ReadinessEvidence{}}
	agent := "unknown"
	var job map[string]any
	shouldStore := false
	defer func() {
		if recover() != nil {
			verdict = review.ReadinessVerdict{Verdict: "UNKNOWN", Evidence: []review.ReadinessEvidence{}}
		}
		if job != nil {
			enforce.RecordReadinessOutcome(filepath.Dir(filepath.Dir(dir)), job, verdict.Verdict, agent, time.Since(started).Milliseconds())
		}
		if shouldStore {
			storeReadinessVerdict(dir, jobID, verdict)
		}
	}()

	record, err := Read(filepath.Join(dir, jobID+".json"))
	if err != nil {
		return
	}
	job = recordMap(record)
	if stringValue(job["jobId"]) != jobID {
		job = nil
		return
	}
	readiness, ok := job["readinessCheck"].(map[string]any)
	if !ok || readiness["grounded"] != true {
		job = nil // The Python host returns before telemetry for an ungrounded check.
		return
	}
	shouldStore = true
	agent, _ = readiness["agent"].(string)
	if agent == "" {
		agent = "unknown"
	}
	if agent == "off" {
		verdict.Verdict = "UNAVAILABLE"
		return
	}
	worktree, worktreeOK := job["worktreePath"].(string)
	snapshot, snapshotOK := job["snapshot"].(string)
	objective, objectiveOK := readiness["objective"].(string)
	acceptance, acceptanceOK := readiness["acceptance_criteria"].(string)
	model, _ := readiness["model"].(string)
	if !worktreeOK || !snapshotOK || !objectiveOK || !acceptanceOK {
		return
	}
	if review.SameFamily(readinessModelFamily(stringValue(job["model"])), readinessModelFamily(model)) {
		verdict.Verdict = "UNAVAILABLE"
		return
	}

	config, err := readinessCoderConfig()
	if err != nil {
		return
	}
	delta := ledger.ComputeJobDelta(worktree, snapshot)
	if delta["status"] != "OK" {
		return
	}
	delta, err = ledger.FilterJobDeltaChecked(worktree, delta)
	if err != nil || delta["status"] != "OK" {
		return
	}
	_, attributedDiff, err := review.DelegatedDiff(worktree, delta)
	if err != nil {
		return
	}
	workerReport, _ := result["content"].(string)
	repoRoot, _ := job["repoRoot"].(string)
	if repoRoot == "" {
		repoRoot = worktree
	}
	executable, _ := config["reviewer1_executable"].(string)
	got, err := review.RunReadiness(context.Background(), obsidian.PluginRoot(), review.ReadinessRequest{
		Grounded: true, Objective: objective, AcceptanceCriteria: acceptance, AttributedDiff: attributedDiff,
		WorkerReport: workerReport, Agent: agent, Model: model, RepoRoot: repoRoot, Executable: executable,
	}, 90*time.Second)
	if err == nil && validStoredReadiness(got) {
		verdict = got
	}
}

func validStoredReadiness(verdict review.ReadinessVerdict) bool {
	return verdict.Verdict == "UNMET" || verdict.Verdict == "NO_UNMET_FOUND" || verdict.Verdict == "UNKNOWN" || verdict.Verdict == "UNAVAILABLE"
}

func storeReadinessVerdict(dir, jobID string, verdict review.ReadinessVerdict) {
	lock, err := AcquireOwnership(dir, jobID)
	if err != nil {
		return
	}
	defer lock.Release()
	record, err := Read(filepath.Join(dir, jobID+".json"))
	if err != nil {
		return
	}
	var state string
	var recordedID string
	var readiness map[string]any
	record.Get("state", &state)
	record.Get("jobId", &recordedID)
	if recordedID != jobID || state != "RUNNING" || !record.Get("readinessCheck", &readiness) || readiness == nil {
		return
	}
	encoded, err := json.Marshal(verdict)
	if err != nil {
		return
	}
	var result map[string]any
	if json.Unmarshal(encoded, &result) != nil {
		return
	}
	readiness["result"] = result
	readiness["checkedAt"] = float64(time.Now().UnixNano()) / 1e9
	if record.Set("readinessCheck", readiness) == nil {
		_ = Write(filepath.Join(dir, jobID+".json"), record)
	}
}

func readinessCoderConfig() (map[string]any, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return policy.LoadCoderConfig(obsidian.ConfigPath(), filepath.Join(home, ".orchestrator", "config.toml"))
}

var modelVendorPrefix = regexp.MustCompile(`^(openai|google|deepseek|anthropic|qwen|mistral|llama)/`)

func readinessModelFamily(model string) string {
	name := strings.ToLower(model)
	if match := modelVendorPrefix.FindStringSubmatch(name); len(match) == 2 {
		if match[1] == "anthropic" {
			return "claude"
		}
		return match[1]
	}
	if index := strings.Index(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	switch {
	case strings.HasPrefix(name, "gemini-"):
		return "gemini"
	case strings.HasPrefix(name, "gpt-") || regexp.MustCompile(`^o[134](-|$)`).MatchString(name):
		return "gpt"
	case strings.HasPrefix(name, "claude-"):
		return "claude"
	case strings.HasPrefix(name, "deepseek-"):
		return "deepseek"
	case strings.HasPrefix(name, "qwen-") || regexp.MustCompile(`^qwen[0-9]`).MatchString(name):
		return "qwen"
	case strings.HasPrefix(name, "mistral-") || regexp.MustCompile(`^mistral[0-9]`).MatchString(name):
		return "mistral"
	case strings.HasPrefix(name, "llama-") || regexp.MustCompile(`^llama[0-9]`).MatchString(name):
		return "llama"
	default:
		return ""
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
