package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const planSchemaVersion = 1

type PlanState struct {
	Version             int    `json:"version"`
	Revision            int    `json:"revision"`
	ProjectKey          string `json:"project_key"`
	ProjectRoot         string `json:"project_root"`
	TaskID              string `json:"task_id"`
	ArtifactPath        string `json:"artifact_path"`
	ArtifactDigest      string `json:"artifact_digest"`
	AcceptedMilestones  []int  `json:"accepted_milestones"`
	CompletedMilestones []int  `json:"completed_milestones"`
	CurrentMilestone    int    `json:"current_milestone"`
	LinkedJob           string `json:"linked_job"`
	PendingNextAction   string `json:"pending_next_action"`
	UserStopped         bool   `json:"user_stopped"`
	Gate                string `json:"gate"`
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
		if state.CurrentMilestone == 0 {
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
