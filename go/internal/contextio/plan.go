package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const planSchemaVersion = 1

type PlanState struct {
	Version             int        `json:"version"`
	Revision            int        `json:"revision"`
	ProjectKey          string     `json:"project_key"`
	ProjectRoot         string     `json:"project_root"`
	TaskID              string     `json:"task_id"`
	ArtifactPath        string     `json:"artifact_path"`
	ArtifactDigest      string     `json:"artifact_digest"`
	AcceptedMilestones  []int      `json:"accepted_milestones"`
	CompletedMilestones []int      `json:"completed_milestones"`
	CurrentMilestone    int        `json:"current_milestone"`
	LinkedJobs          []string   `json:"linked_jobs,omitempty"`
	LinkedJob           string     `json:"linked_job,omitempty"`
	PendingNextAction   string     `json:"pending_next_action"`
	UserStopped         bool       `json:"user_stopped"`
	Gate                string     `json:"gate"`
	AppliedPasses       []PlanPass `json:"applied_passes,omitempty"`
}

type PlanPass struct {
	Milestone int    `json:"milestone"`
	TaskID    string `json:"task_id"`
	JobID     string `json:"job_id"`
}

var writePlanJSONAtomically = writeJSONAtomically

func planPath(root, repository string) (string, string, error) {
	canonical := ProjectRoot(map[string]any{"cwd": repository})
	if canonical == "" {
		return "", "", errors.New("repository path is not inside a Git repository")
	}
	key := ProjectKey(canonical)
	return filepath.Join(root, "projects", key, "active_plan.json"), canonical, nil
}

func normalizePlanPath(path string) (string, error) {
	path = strings.ReplaceAll(path, `\`, string(filepath.Separator))
	path = strings.ReplaceAll(path, "/", string(filepath.Separator))
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = resolve(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return filepath.Clean(abs), nil
}

func readPlan(path string) (*PlanState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state PlanState
	if err := json.Unmarshal(data, &state); err != nil || state.Version != planSchemaVersion || state.Revision < 1 || state.TaskID == "" || state.ProjectKey == "" || state.ProjectRoot == "" || state.ArtifactPath == "" || len(state.ArtifactDigest) != 64 || len(state.AcceptedMilestones) == 0 {
		return nil, errors.New("active plan record is malformed")
	}
	if state.LinkedJob != "" && !containsPlanString(state.LinkedJobs, state.LinkedJob) {
		state.LinkedJobs = append(state.LinkedJobs, state.LinkedJob)
	}
	if len(state.LinkedJobs) != 0 {
		state.PendingNextAction = "apply"
	}
	if _, err := hex.DecodeString(state.ArtifactDigest); err != nil {
		return nil, errors.New("active plan record is malformed")
	}
	seen := make(map[int]bool, len(state.AcceptedMilestones))
	for _, milestone := range state.AcceptedMilestones {
		if milestone < 1 || seen[milestone] {
			return nil, errors.New("active plan record is malformed")
		}
		seen[milestone] = true
	}
	completed := make(map[int]bool, len(state.CompletedMilestones))
	for _, milestone := range state.CompletedMilestones {
		if !seen[milestone] || completed[milestone] {
			return nil, errors.New("active plan record is malformed")
		}
		completed[milestone] = true
	}
	if state.CurrentMilestone != 0 && (!seen[state.CurrentMilestone] || completed[state.CurrentMilestone]) {
		return nil, errors.New("active plan record is malformed")
	}
	artifact, err := os.ReadFile(state.ArtifactPath)
	if err != nil {
		return nil, fmt.Errorf("active plan artifact is unavailable: %w", err)
	}
	digest := sha256.Sum256(artifact)
	if hex.EncodeToString(digest[:]) != state.ArtifactDigest {
		return nil, errors.New("active plan artifact digest does not match")
	}
	return &state, nil
}

func ReadPlan(root, repository string) (*PlanState, error) {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := readPlan(path)
	if err != nil {
		return nil, err
	}
	if state.ProjectKey != ProjectKey(canonical) || !samePlanPath(state.ProjectRoot, canonical) {
		return nil, errors.New("active plan belongs to a different project path")
	}
	return state, nil
}

func AcceptPlan(root, repository, taskID, artifact string, milestones []int) (*PlanState, error) {
	if taskID == "" || len(milestones) == 0 {
		return nil, errors.New("task id and accepted milestones are required")
	}
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, err
	}
	artifactPath, err := normalizePlanPath(artifact)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(artifactPath)
	if err != nil {
		return nil, fmt.Errorf("read plan artifact: %w", err)
	}
	digest := sha256.Sum256(data)
	accepted := append([]int(nil), milestones...)
	sort.Ints(accepted)
	for i, milestone := range accepted {
		if milestone < 1 || (i > 0 && accepted[i-1] == milestone) {
			return nil, errors.New("accepted milestones must be unique positive numbers")
		}
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if existing, err := readPlan(path); err == nil {
		if existing.ProjectKey != ProjectKey(canonical) || !samePlanPath(existing.ProjectRoot, canonical) {
			return nil, errors.New("active plan belongs to a different project path")
		}
		if existing.CurrentMilestone != 0 {
			return nil, errors.New("an unfinished plan is already active for this project")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	state := &PlanState{Version: planSchemaVersion, Revision: 1, ProjectKey: ProjectKey(canonical), ProjectRoot: canonical, TaskID: taskID, ArtifactPath: artifactPath, ArtifactDigest: hex.EncodeToString(digest[:]), AcceptedMilestones: accepted, CurrentMilestone: accepted[0], PendingNextAction: "dispatch"}
	if err := writePlanJSONAtomically(path, state); err != nil {
		return nil, err
	}
	return state, nil
}

func UpdatePlan(root, repository, taskID string, expectedRevision int, operation, value string) (*PlanState, error) {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := readPlan(path)
	if err != nil {
		return nil, err
	}
	if state.ProjectKey != ProjectKey(canonical) || !samePlanPath(state.ProjectRoot, canonical) {
		return nil, errors.New("active plan belongs to a different project path")
	}
	if taskID != "" && state.TaskID != taskID {
		return nil, errors.New("plan task id does not match active plan")
	}
	if expectedRevision != state.Revision {
		return nil, fmt.Errorf("stale plan update: current revision is %d", state.Revision)
	}
	switch operation {
	case "stop":
		state.UserStopped = true
	case "resume":
		state.UserStopped = false
	case "gate":
		state.Gate = value
	case "complete-milestone":
		if state.CurrentMilestone == 0 || hasOpenLinkedJob(root, state, state.CurrentMilestone) {
			return nil, errors.New("plan has no current milestone")
		}
		if !containsMilestone(state.CompletedMilestones, state.CurrentMilestone) {
			state.CompletedMilestones = append(state.CompletedMilestones, state.CurrentMilestone)
		}
		state.CurrentMilestone = 0
		for _, milestone := range state.AcceptedMilestones {
			if !containsMilestone(state.CompletedMilestones, milestone) {
				state.CurrentMilestone = milestone
				break
			}
		}
		if state.CurrentMilestone == 0 {
			state.PendingNextAction = ""
		} else {
			state.PendingNextAction = "dispatch"
		}
	default:
		return nil, errors.New("unsupported plan operation")
	}
	state.Revision++
	if err := writePlanJSONAtomically(path, state); err != nil {
		return nil, err
	}
	return state, nil
}

var planTaskSuffix = regexp.MustCompile(`^([0-9]+)of([0-9]+)(?:-p[0-9]+[a-z]?)?(?:-[a-z][a-z0-9-]*)?$`)
var legacyPlanTaskSuffix = regexp.MustCompile(`^([0-9]+)(?:-p[0-9]+[a-z]?)?(?:-[a-z][a-z0-9-]*)?$`)

// BindPlanJob links a pass to its accepted, unfinished milestone. The plan
// lock serializes updates, but bindings do not require a caller revision:
// independent passes may be linked concurrently.
func BindPlanJob(root, repository, taskID, jobID string) (*PlanState, bool, error) {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, false, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	state, err := readPlan(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if state.ProjectKey != ProjectKey(canonical) || !samePlanPath(state.ProjectRoot, canonical) {
		return nil, false, errors.New("active plan belongs to a different project path")
	}
	milestone, matches := planTaskMilestone(state.TaskID, taskID)
	if !matches {
		return state, false, nil
	}
	if !containsMilestone(state.AcceptedMilestones, milestone) || containsMilestone(state.CompletedMilestones, milestone) || state.UserStopped || state.Gate != "" {
		return nil, true, errors.New("active plan is not ready for this dispatch")
	}
	linked := linkedJobs(state)
	if !containsPlanString(linked, jobID) {
		linked = append(linked, jobID)
	}
	setLinkedJobs(state, linked)
	state.PendingNextAction = "apply"
	state.Revision++
	if err := writePlanJSONAtomically(path, state); err != nil {
		return nil, true, err
	}
	return state, true, nil
}

// RecordPlanApply records an explicitly applied pass. It never advances the
// milestone; only UpdatePlan(..., "complete-milestone", ...) can do that.
func RecordPlanApply(root, repository, taskID, jobID string) error {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	state, err := readPlan(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.ProjectKey != ProjectKey(canonical) || !samePlanPath(state.ProjectRoot, canonical) {
		return errors.New("active plan belongs to a different project path")
	}
	milestone, matches := planTaskMilestone(state.TaskID, taskID)
	if !matches {
		return nil
	}
	for _, pass := range state.AppliedPasses {
		if pass.JobID == jobID {
			return nil
		}
	}
	linked := linkedJobs(state)
	if !containsPlanString(linked, jobID) {
		return errors.New("applied job is not linked to the current plan action")
	}
	state.AppliedPasses = append(state.AppliedPasses, PlanPass{Milestone: milestone, TaskID: taskID, JobID: jobID})
	linked = removeString(linked, jobID)
	setLinkedJobs(state, linked)
	state.PendingNextAction = pendingAction(state)
	state.Revision++
	return writePlanJSONAtomically(path, state)
}

// ReconcilePlanJob clears a missing or failed link and recovers an applied pass
// if the job write landed before the plan progress write.
func ReconcilePlanJob(root, repository string) (*PlanState, error) {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := readPlan(path)
	if err != nil {
		return nil, err
	}
	if state.ProjectKey != ProjectKey(canonical) || !samePlanPath(state.ProjectRoot, canonical) {
		return nil, errors.New("active plan belongs to a different project path")
	}
	linked := linkedJobs(state)
	if len(linked) == 0 {
		state.PendingNextAction = "dispatch"
		return state, nil
	}
	changed := false
	remaining := make([]string, 0, len(linked))
	for _, jobID := range linked {
		jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
		data, readErr := os.ReadFile(jobPath)
		var job map[string]any
		if readErr == nil {
			readErr = json.Unmarshal(data, &job)
		}
		if readErr != nil || job["jobId"] != jobID {
			changed = true
			continue
		}
		if job["applyState"] == "applied" {
			taskID, _ := job["taskId"].(string)
			milestone, matches := planTaskMilestone(state.TaskID, taskID)
			if matches && containsMilestone(state.AcceptedMilestones, milestone) && !hasAppliedJob(state, jobID) {
				state.AppliedPasses = append(state.AppliedPasses, PlanPass{Milestone: milestone, TaskID: taskID, JobID: jobID})
			}
			changed = true
			continue
		}
		stateText, _ := job["state"].(string)
		if stateText == "FAILED" || stateText == "CANCELLED" || stateText == "TIMED_OUT" {
			changed = true
			continue
		}
		remaining = append(remaining, jobID)
	}
	setLinkedJobs(state, remaining)
	if state.PendingNextAction != pendingAction(state) {
		changed = true
	}
	state.PendingNextAction = pendingAction(state)
	if !changed {
		return state, nil
	}
	state.Revision++
	if err := writePlanJSONAtomically(path, state); err != nil {
		return nil, err
	}
	return state, nil
}

func planTaskMilestone(planID, taskID string) (int, bool) {
	if !strings.HasPrefix(taskID, planID+"-m") {
		return 0, false
	}
	suffix := strings.TrimPrefix(taskID, planID+"-m")
	if match := planTaskSuffix.FindStringSubmatch(suffix); match != nil {
		milestone, err := strconv.Atoi(match[1])
		return milestone, err == nil
	}
	if match := legacyPlanTaskSuffix.FindStringSubmatch(suffix); match != nil {
		milestone, err := strconv.Atoi(match[1])
		return milestone, err == nil
	}
	return 0, false
}

// PlanTaskMatches reports whether taskID names a pass under this exact plan ID.
func PlanTaskMatches(planID, taskID string) bool {
	_, ok := planTaskMilestone(planID, taskID)
	return ok
}

func hasAppliedJob(state *PlanState, jobID string) bool {
	for _, pass := range state.AppliedPasses {
		if pass.JobID == jobID {
			return true
		}
	}
	return false
}

func linkedJobs(state *PlanState) []string {
	jobs := append([]string(nil), state.LinkedJobs...)
	if state.LinkedJob != "" && !containsPlanString(jobs, state.LinkedJob) {
		jobs = append(jobs, state.LinkedJob)
	}
	return jobs
}

func setLinkedJobs(state *PlanState, jobs []string) {
	state.LinkedJobs = append([]string(nil), jobs...)
	state.LinkedJob = ""
}

func pendingAction(state *PlanState) string {
	if len(linkedJobs(state)) != 0 {
		return "apply"
	}
	return "dispatch"
}

func hasOpenLinkedJob(root string, state *PlanState, milestone int) bool {
	for _, jobID := range linkedJobs(state) {
		data, err := os.ReadFile(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"))
		if err != nil {
			return true
		}
		var job map[string]any
		if json.Unmarshal(data, &job) != nil || job["jobId"] != jobID {
			return true
		}
		taskID, _ := job["taskId"].(string)
		jobMilestone, matches := planTaskMilestone(state.TaskID, taskID)
		if !matches || jobMilestone != milestone {
			continue
		}
		if job["applyState"] == "applied" {
			continue
		}
		stateText, _ := job["state"].(string)
		if stateText != "FAILED" && stateText != "CANCELLED" && stateText != "TIMED_OUT" {
			return true
		}
	}
	return false
}

func hasOpenPlanTaskJob(root string, state *PlanState) bool {
	jobsDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		job, ok := readSmallObject(filepath.Join(jobsDir, entry.Name()))
		if !ok || job["jobId"] != strings.TrimSuffix(entry.Name(), ".json") {
			continue
		}
		taskID, _ := job["taskId"].(string)
		if taskID != state.TaskID && !PlanTaskMatches(state.TaskID, taskID) {
			continue
		}
		if job["state"] == "QUEUED" || job["state"] == "RUNNING" {
			return true
		}
	}
	return false
}

func containsPlanString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func removeString(values []string, value string) []string {
	for i, candidate := range values {
		if candidate == value {
			return append(values[:i], values[i+1:]...)
		}
	}
	return values
}

func samePlanPath(a, b string) bool {
	a, _ = normalizePlanPath(a)
	b, _ = normalizePlanPath(b)
	return a == b
}

func containsMilestone(values []int, milestone int) bool {
	for _, value := range values {
		if value == milestone {
			return true
		}
	}
	return false
}
