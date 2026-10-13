package contextio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const planSchemaVersion = 1
const planCollectionVersion = 2

var ErrPlanNotReady = errors.New("active plan is not ready for this dispatch")

type PlanReadinessError struct {
	MissingDependencies []string
}

func (e *PlanReadinessError) Error() string {
	if len(e.MissingDependencies) > 0 {
		return fmt.Sprintf("active plan is not ready: dependency %s is not done", strings.Join(e.MissingDependencies, ", "))
	}
	return ErrPlanNotReady.Error()
}

func (e *PlanReadinessError) Is(target error) bool { return target == ErrPlanNotReady }

var knownPlanValidationStates = map[string]struct{}{
	"PENDING": {}, "PASS": {}, "FAILED": {}, "FAIL": {}, "UNKNOWN": {},
	"STALE": {}, "LAUNCH_ERROR": {}, "INTERRUPTED": {}, "TIMEOUT": {}, "SKIPPED": {},
}

type PlanState struct {
	Version             int              `json:"version"`
	Revision            int              `json:"revision"`
	ProjectKey          string           `json:"project_key"`
	ProjectRoot         string           `json:"project_root"`
	TaskID              string           `json:"task_id"`
	ArtifactPath        string           `json:"artifact_path"`
	ArtifactDigest      string           `json:"artifact_digest"`
	AcceptedMilestones  []int            `json:"accepted_milestones"`
	CompletedMilestones []int            `json:"completed_milestones"`
	CurrentMilestone    int              `json:"current_milestone"`
	LinkedJobs          []string         `json:"linked_jobs,omitempty"`
	LinkedJob           string           `json:"linked_job,omitempty"`
	PendingNextAction   string           `json:"pending_next_action"`
	UserStopped         bool             `json:"user_stopped"`
	Gate                string           `json:"gate"`
	AppliedPasses       []PlanPass       `json:"applied_passes,omitempty"`
	Milestones          []PlanMilestone  `json:"milestones,omitempty"`
	ReadyMilestones     []ReadyMilestone `json:"ready_milestones,omitempty"`
}

type PlanMilestone struct {
	ID              string   `json:"id"`
	Dependencies    []int    `json:"dependencies,omitempty"`
	WritePaths      []string `json:"write_paths,omitempty"`
	TaskSection     string   `json:"task_section"`
	ValidationState string   `json:"validation_state,omitempty"`
}

type ReadyMilestone struct {
	ID          string `json:"id"`
	ArtifactRef string `json:"artifact_ref"`
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

type planCollection struct {
	Version int         `json:"version"`
	Plans   []PlanState `json:"plans"`
}

func validatePlanRecord(state *PlanState) error {
	if state.Version != planSchemaVersion || state.Revision < 1 || state.TaskID == "" || state.ProjectKey == "" || state.ProjectRoot == "" || state.ArtifactPath == "" || len(state.ArtifactDigest) != 64 || len(state.AcceptedMilestones) == 0 {
		return errors.New("active plan record is malformed")
	}
	if state.LinkedJob != "" && !containsPlanString(state.LinkedJobs, state.LinkedJob) {
		state.LinkedJobs = append(state.LinkedJobs, state.LinkedJob)
	}
	if state.CurrentMilestone == 0 {
		state.PendingNextAction = ""
	} else if len(state.LinkedJobs) != 0 {
		state.PendingNextAction = "apply"
	} else {
		state.PendingNextAction = "dispatch"
	}
	if _, err := hex.DecodeString(state.ArtifactDigest); err != nil {
		return errors.New("active plan record is malformed")
	}
	seen := make(map[int]bool, len(state.AcceptedMilestones))
	for _, milestone := range state.AcceptedMilestones {
		if milestone < 1 || seen[milestone] {
			return errors.New("active plan record is malformed")
		}
		seen[milestone] = true
	}
	completed := make(map[int]bool, len(state.CompletedMilestones))
	for _, milestone := range state.CompletedMilestones {
		if !seen[milestone] || completed[milestone] {
			return errors.New("active plan record is malformed")
		}
		completed[milestone] = true
	}
	if state.CurrentMilestone != 0 && (!seen[state.CurrentMilestone] || completed[state.CurrentMilestone]) {
		return errors.New("active plan record is malformed")
	}
	linked := make(map[string]bool, len(state.LinkedJobs))
	for _, jobID := range state.LinkedJobs {
		if jobID == "" || linked[jobID] {
			return errors.New("active plan record is malformed")
		}
		linked[jobID] = true
	}
	applied := make(map[string]bool, len(state.AppliedPasses))
	for _, pass := range state.AppliedPasses {
		milestone, matches := planTaskMilestone(state.TaskID, pass.TaskID)
		if pass.JobID == "" || pass.TaskID == "" || applied[pass.JobID] || !matches || milestone != pass.Milestone || !seen[milestone] {
			return errors.New("active plan record is malformed")
		}
		applied[pass.JobID] = true
	}
	for _, milestone := range state.Milestones {
		id, err := strconv.Atoi(strings.TrimPrefix(milestone.ID, "M"))
		if err != nil || !seen[id] || milestone.TaskSection == "" {
			return errors.New("active plan record is malformed")
		}
		for _, dependency := range milestone.Dependencies {
			if !seen[dependency] || dependency == id {
				return errors.New("active plan record is malformed")
			}
		}
	}
	return nil
}

func validatePlanArtifact(state *PlanState) error {
	artifact, err := os.ReadFile(state.ArtifactPath)
	if err != nil {
		return fmt.Errorf("active plan artifact is unavailable: %w", err)
	}
	digest := sha256.Sum256(artifact)
	if hex.EncodeToString(digest[:]) != state.ArtifactDigest {
		return errors.New("active plan artifact digest does not match")
	}
	if len(state.Milestones) > 0 {
		expected, err := parsePlanMilestones(artifact, state.AcceptedMilestones)
		if err != nil || len(expected) != len(state.Milestones) {
			return errors.New("active plan metadata does not match artifact")
		}
		for i, m := range expected {
			saved := state.Milestones[i]
			saved.ValidationState = ""
			if !reflect.DeepEqual(m, saved) {
				return errors.New("active plan metadata does not match artifact")
			}
		}
	}
	return nil
}

func parsePlanMilestones(data []byte, accepted []int) ([]PlanMilestone, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	headings := regexp.MustCompile(`(?m)^(#{1,6})\s+([^\n]+)(?:\n|$)`).FindAllStringSubmatchIndex(text, -1)
	idPattern := regexp.MustCompile(`^M([0-9]+)\b`)
	depLine := regexp.MustCompile(`(?im)^[ \t]*(?:[-*][ \t]+)?\*\*Dependenc(?:y|ies):\*\*[ \t]*([^\n]*)`)
	pathLine := regexp.MustCompile(`(?im)^[ \t]*(?:[-*][ \t]+)?\*\*(?:Touched paths|Touched files/dirs|Write paths):\*\*[ \t]*([^\n]*)`)
	quotedPath := regexp.MustCompile("`([^`]+)`")
	var result []PlanMilestone
	explicit := false
	for i, heading := range headings {
		match := idPattern.FindStringSubmatch(text[heading[4]:heading[5]])
		if match == nil {
			continue
		}
		n, err := strconv.Atoi(match[1])
		if err != nil || n < 1 || strconv.Itoa(n) != match[1] {
			return nil, errors.New("invalid milestone ID")
		}
		end := len(text)
		for _, next := range headings[i+1:] {
			if next[3]-next[2] <= heading[3]-heading[2] {
				end = next[0]
				break
			}
		}
		section := strings.TrimSpace(text[heading[0]:end])
		if len(section) > 32768 || len(result) >= 128 {
			return nil, errors.New("plan task sections exceed bounds")
		}
		m := PlanMilestone{ID: "M" + match[1], TaskSection: section}
		declarations := depLine.FindAllStringSubmatch(section, -1)
		if len(declarations) > 1 {
			return nil, fmt.Errorf("duplicate dependency declaration for %s", m.ID)
		}
		if len(declarations) == 1 {
			explicit = true
			// Parallel-group annotations are not dependency declarations.
			raw := strings.TrimSuffix(strings.TrimSpace(strings.SplitN(declarations[0][1], ". **Parallel group:**", 2)[0]), ".")
			if !strings.EqualFold(raw, "none") {
				for _, token := range strings.Split(raw, ",") {
					n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(token), "M"))
					if err != nil || n < 1 || containsMilestone(m.Dependencies, n) {
						return nil, fmt.Errorf("invalid dependencies for %s", m.ID)
					}
					m.Dependencies = append(m.Dependencies, n)
				}
			}
		}
		if declaration := pathLine.FindStringSubmatchIndex(section); declaration != nil {
			inline := section[declaration[2]:declaration[3]]
			lines := []string{}
			if paths := quotedPath.FindAllStringSubmatch(inline, -1); len(paths) > 0 {
				for _, path := range paths {
					lines = append(lines, "- `"+path[1]+"`")
				}
			} else {
				lines = strings.Split(section[declaration[1]:], "\n")[1:]
			}
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") {
					break
				}
				if strings.HasPrefix(strings.TrimSpace(line[2:]), "**") {
					break
				}
				paths := quotedPath.FindAllStringSubmatch(line, -1)
				if len(paths) == 0 {
					paths = [][]string{{"", strings.TrimSpace(line[2:])}}
				}
				for _, path := range paths {
					clean := filepath.ToSlash(filepath.Clean(strings.ReplaceAll(path[1], `\`, "/")))
					if clean == "." || filepath.IsAbs(clean) || strings.Contains(clean, ":") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(clean, "*?[]") {
						return nil, fmt.Errorf("invalid write path for %s", m.ID)
					}
					if runtime.GOOS == "windows" {
						clean = strings.ToLower(clean)
					}
					m.WritePaths = append(m.WritePaths, clean)
				}
			}
		}
		result = append(result, m)
	}
	if !explicit {
		return nil, nil
	} // Undeclared legacy dependencies stay serial.
	byID := map[int]PlanMilestone{}
	for _, m := range result {
		n := milestoneNumber(m.ID)
		if _, duplicate := byID[n]; duplicate || depLine.FindStringSubmatch(m.TaskSection) == nil || len(m.WritePaths) == 0 {
			return nil, fmt.Errorf("duplicate or incomplete milestone %s", m.ID)
		}
		byID[n] = m
	}
	for _, n := range accepted {
		if _, ok := byID[n]; !ok {
			return nil, fmt.Errorf("accepted milestone M%d is missing", n)
		}
	}
	visiting, visited := map[int]bool{}, map[int]bool{}
	ancestors := map[int]map[int]bool{}
	var visit func(int) error
	visit = func(n int) error {
		if visiting[n] {
			return errors.New("plan milestone dependencies contain a cycle")
		}
		if visited[n] {
			return nil
		}
		visiting[n] = true
		ancestors[n] = map[int]bool{}
		for _, dep := range byID[n].Dependencies {
			if _, ok := byID[dep]; !ok {
				return fmt.Errorf("milestone M%d has missing dependency M%d", n, dep)
			}
			if containsMilestone(accepted, n) && !containsMilestone(accepted, dep) {
				return fmt.Errorf("milestone M%d requires unaccepted M%d", n, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
			ancestors[n][dep] = true
			for ancestor := range ancestors[dep] {
				ancestors[n][ancestor] = true
			}
		}
		visiting[n], visited[n] = false, true
		return nil
	}
	for n := range byID {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	for i, n := range accepted {
		for _, other := range accepted[i+1:] {
			if ancestors[n][other] || ancestors[other][n] {
				continue
			}
			for _, a := range byID[n].WritePaths {
				for _, b := range byID[other].WritePaths {
					if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
						return nil, fmt.Errorf("parallel milestones M%d and M%d have incompatible write paths", n, other)
					}
				}
			}
		}
	}
	selected := make([]PlanMilestone, 0, len(accepted))
	for _, n := range accepted {
		selected = append(selected, byID[n])
	}
	return selected, nil
}

func milestoneNumber(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "M"))
	return n
}

func readPlans(path string) ([]*PlanState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, errors.New("active plan record is malformed")
	}
	var plans []PlanState
	switch header.Version {
	case planSchemaVersion:
		var legacy PlanState
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, errors.New("active plan record is malformed")
		}
		plans = []PlanState{legacy}
	case planCollectionVersion:
		var collection planCollection
		if err := json.Unmarshal(data, &collection); err != nil || collection.Version != planCollectionVersion || collection.Plans == nil {
			return nil, errors.New("active plan collection is malformed")
		}
		plans = collection.Plans
	default:
		return nil, fmt.Errorf("unsupported active plan version %d", header.Version)
	}
	states := make([]*PlanState, 0, len(plans))
	ids := make(map[string]bool, len(plans))
	for i := range plans {
		state := &plans[i]
		if err := validatePlanRecord(state); err != nil {
			return nil, err
		}
		if ids[state.TaskID] {
			return nil, fmt.Errorf("duplicate active plan task id %q", state.TaskID)
		}
		ids[state.TaskID] = true
		states = append(states, state)
	}
	if len(states) == 0 {
		return nil, errors.New("active plan collection is empty")
	}
	return states, nil
}

func selectPlan(states []*PlanState, taskID string) (*PlanState, error) {
	if taskID != "" {
		for _, state := range states {
			if state.TaskID == taskID {
				return state, nil
			}
		}
		return nil, fmt.Errorf("active plan task id %q was not found", taskID)
	}
	var selected *PlanState
	for _, state := range states {
		if state.CurrentMilestone == 0 {
			continue
		}
		if selected != nil {
			return nil, errors.New("multiple unfinished plans; specify a plan task id")
		}
		selected = state
	}
	if selected == nil {
		return nil, errors.New("no unfinished plan; specify a plan task id")
	}
	return selected, nil
}

func checkPlanProjects(states []*PlanState, canonical string) error {
	for _, state := range states {
		if state.ProjectKey != ProjectKey(canonical) || !samePlanPath(state.ProjectRoot, canonical) {
			return errors.New("active plan belongs to a different project path")
		}
	}
	return nil
}

func writePlans(path string, states []*PlanState) error {
	collection := planCollection{Version: planCollectionVersion, Plans: make([]PlanState, 0, len(states))}
	for _, state := range states {
		collection.Plans = append(collection.Plans, *state)
	}
	return writePlanJSONAtomically(path, collection)
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
	states, err := readPlans(path)
	if err != nil {
		return nil, err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return nil, err
	}
	state, err := selectPlan(states, "")
	if err != nil {
		return nil, err
	}
	if err := validatePlanArtifact(state); err != nil {
		return nil, err
	}
	state.ReadyMilestones = readyMilestones(root, state)
	return state, nil
}

// ReadPlanForTask returns one exact plan record, including stopped or completed plans.
func ReadPlanForTask(root, repository, taskID string) (*PlanState, error) {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	states, err := readPlans(path)
	if err != nil {
		return nil, err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return nil, err
	}
	state, err := selectPlan(states, taskID)
	if err != nil {
		return nil, err
	}
	if err := validatePlanArtifact(state); err != nil {
		return nil, err
	}
	state.ReadyMilestones = readyMilestones(root, state)
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
	states, err := readPlans(path)
	if err == nil {
		if err := checkPlanProjects(states, canonical); err != nil {
			return nil, err
		}
		for _, existing := range states {
			if existing.TaskID == taskID {
				return nil, fmt.Errorf("active plan task id %q already exists", taskID)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else {
		states = nil
	}
	metadata, err := parsePlanMilestones(data, accepted)
	if err != nil {
		return nil, err
	}
	state := &PlanState{Version: planSchemaVersion, Revision: 1, ProjectKey: ProjectKey(canonical), ProjectRoot: canonical, TaskID: taskID, ArtifactPath: artifactPath, ArtifactDigest: hex.EncodeToString(digest[:]), AcceptedMilestones: accepted, CurrentMilestone: accepted[0], PendingNextAction: "dispatch", Milestones: metadata}
	state.ReadyMilestones = readyMilestones(root, state)
	states = append(states, state)
	if err := writePlans(path, states); err != nil {
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
	states, err := readPlans(path)
	if err != nil {
		return nil, err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return nil, err
	}
	state, err := selectPlan(states, taskID)
	if err != nil {
		return nil, err
	}
	if err := validatePlanArtifact(state); err != nil {
		return nil, err
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
		milestone := state.CurrentMilestone
		if value != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(value, "M"))
			if err != nil || !containsMilestone(state.AcceptedMilestones, n) {
				return nil, errors.New("invalid accepted milestone number")
			}
			milestone = n
		} else {
			// A legacy follow-up after a plain milestone apply must not finish its successor.
			for i := len(state.AppliedPasses) - 1; i >= 0; i-- {
				pass := state.AppliedPasses[i]
				if plainPlanTaskMilestone(state.TaskID, pass.TaskID, pass.Milestone) && containsMilestone(state.CompletedMilestones, pass.Milestone) {
					return state, nil
				}
			}
		}
		if containsMilestone(state.CompletedMilestones, milestone) {
			return state, nil
		}
		if milestone == 0 {
			return nil, errors.New("plan has no current milestone")
		}
		if len(state.Milestones) > 0 && !planDependenciesDone(state, milestone) {
			return nil, errors.New("milestone prerequisites are not complete and validated")
		}
		if jobID := runningLinkedJob(root, state, milestone); jobID != "" {
			return nil, fmt.Errorf("milestone %d has a running linked job %s", milestone, jobID)
		}
		state.CompletedMilestones = append(state.CompletedMilestones, milestone)
		// Preserve independent work when completing a selected graph milestone.
		if len(state.Milestones) == 0 {
			setLinkedJobs(state, nil)
		}
		state.CurrentMilestone = 0
		for _, n := range state.AcceptedMilestones {
			if !containsMilestone(state.CompletedMilestones, n) {
				state.CurrentMilestone = n
				break
			}
		}
		state.PendingNextAction = pendingAction(state)

	default:
		return nil, errors.New("unsupported plan operation")
	}
	if len(state.Milestones) > 0 {
		state.ReadyMilestones = readyMilestones(root, state)
	}
	state.Revision++
	if err := writePlans(path, states); err != nil {
		return nil, err
	}
	return state, nil
}

var planTaskSuffix = regexp.MustCompile(`^([0-9]+)of([0-9]+)(?:-p[0-9]+[a-z]?)?(?:-[a-z][a-z0-9-]*)?$`)
var legacyPlanTaskSuffix = regexp.MustCompile(`^([0-9]+)(?:-p[0-9]+[a-z]?)?(?:-[a-z][a-z0-9-]*)?$`)
var plainPlanTaskSuffix = regexp.MustCompile(`^([0-9]+)of([0-9]+)$`)

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
	states, err := readPlans(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return nil, false, err
	}
	state, milestone, matches, err := resolvePlanOwner(states, taskID)
	if err != nil {
		return nil, false, err
	}
	if !matches {
		return nil, false, nil
	}
	if err := validatePlanArtifact(state); err != nil {
		return nil, true, err
	}
	if !containsMilestone(state.AcceptedMilestones, milestone) || containsMilestone(state.CompletedMilestones, milestone) || state.UserStopped || state.Gate != "" || (len(state.Milestones) > 0 && !planDependenciesDone(state, milestone)) {
		return nil, true, &PlanReadinessError{MissingDependencies: missingPlanDependencies(state, milestone)}
	}
	linked := linkedJobs(state)
	if !containsPlanString(linked, jobID) {
		linked = append(linked, jobID)
	}
	setLinkedJobs(state, linked)
	state.PendingNextAction = "apply"
	state.ReadyMilestones = readyMilestones(root, state)
	state.Revision++
	if err := writePlans(path, states); err != nil {
		return nil, true, err
	}
	return state, true, nil
}

func missingPlanDependencies(state *PlanState, milestone int) []string {
	for _, item := range state.Milestones {
		if milestoneNumber(item.ID) != milestone {
			continue
		}
		var missing []string
		for _, dependency := range item.Dependencies {
			if !containsMilestone(state.CompletedMilestones, dependency) {
				missing = append(missing, fmt.Sprintf("M%d", dependency))
				continue
			}
			for _, prerequisite := range state.Milestones {
				if milestoneNumber(prerequisite.ID) == dependency && prerequisite.ValidationState != "" && prerequisite.ValidationState != "PASS" {
					missing = append(missing, fmt.Sprintf("M%d", dependency))
					break
				}
			}
		}
		return missing
	}
	return nil
}

// RecordPlanApply records an explicitly applied pass and advances a plain
// milestone job when no linked work remains.
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
	states, err := readPlans(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return err
	}
	state, milestone, matches, err := resolvePlanOwner(states, taskID)
	if err != nil {
		return err
	}
	if !matches {
		return nil
	}
	if err := validatePlanArtifact(state); err != nil {
		return err
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
	for i := range state.Milestones {
		if milestoneNumber(state.Milestones[i].ID) == milestone {
			state.Milestones[i].ValidationState = "PENDING"
		}
	}
	linked = removeString(linked, jobID)
	setLinkedJobs(state, linked)
	state.PendingNextAction = pendingAction(state)
	advanceAppliedMilestone(root, state)
	state.Revision++
	return writePlans(path, states)
}

// RecordPlanValidation updates the M3 readiness predicate for an applied pass.
func RecordPlanValidation(root, repository, taskID, jobID, status string) error {
	if _, ok := knownPlanValidationStates[status]; !ok {
		return fmt.Errorf("invalid plan validation status %q", status)
	}
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	states, err := readPlans(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return err
	}
	state, _, matches, err := resolvePlanOwner(states, taskID)
	if err != nil || !matches {
		return err
	}
	if !hasAppliedJob(state, jobID) {
		return nil
	}
	changed := false
	passIndex := -1
	for i, pass := range state.AppliedPasses {
		if pass.JobID == jobID {
			passIndex = i
			break
		}
	}
	for i := range state.Milestones {
		if milestoneNumber(state.Milestones[i].ID) == milestoneForPass(state, jobID) && state.Milestones[i].ValidationState != status {
			state.Milestones[i].ValidationState = status
			changed = true
		}
	}
	settled := ""
	switch status {
	case "PASS":
		settled = "PASS"
	case "FAILED", "FAIL", "TIMEOUT":
		settled = "FAILED"
	}
	if passIndex > 0 && settled != "" {
		for _, prior := range state.AppliedPasses[:passIndex] {
			for i := range state.Milestones {
				m := &state.Milestones[i]
				if milestoneNumber(m.ID) == prior.Milestone && (m.ValidationState == "STALE" || m.ValidationState == "INTERRUPTED") {
					m.ValidationState = settled
					changed = true
				}
			}
		}
	}
	if changed {
		advanceAppliedMilestone(root, state)
		state.ReadyMilestones = readyMilestones(root, state)
		state.Revision++
		return writePlans(path, states)
	}
	return nil
}

func milestoneForPass(state *PlanState, jobID string) int {
	for _, pass := range state.AppliedPasses {
		if pass.JobID == jobID {
			return pass.Milestone
		}
	}
	return 0
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
	states, err := readPlans(path)
	if err != nil {
		return nil, err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return nil, err
	}
	var state *PlanState
	for _, candidate := range states {
		if len(linkedJobs(candidate)) == 0 {
			continue
		}
		if state != nil {
			return nil, errors.New("multiple plans have jobs to reconcile; select a plan task id")
		}
		state = candidate
	}
	if state == nil {
		var selectErr error
		state, selectErr = selectPlan(states, "")
		if selectErr != nil {
			return nil, selectErr
		}
		if err := validatePlanArtifact(state); err != nil {
			return nil, err
		}
		return state, nil
	}
	if err := validatePlanArtifact(state); err != nil {
		return nil, err
	}
	linked := linkedJobs(state)
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
		taskID, _ := job["taskId"].(string)
		owner, milestone, matches, ownerErr := resolvePlanOwner(states, taskID)
		if ownerErr != nil {
			return nil, ownerErr
		}
		if taskID != "" && (!matches || owner != state) {
			return nil, fmt.Errorf("linked job %q does not belong uniquely to plan %q", jobID, state.TaskID)
		}
		if job["applyState"] == "applied" {
			if matches && owner == state && containsMilestone(state.AcceptedMilestones, milestone) && !hasAppliedJob(state, jobID) {
				state.AppliedPasses = append(state.AppliedPasses, PlanPass{Milestone: milestone, TaskID: taskID, JobID: jobID})
			}
			changed = true
			continue
		}
		stateText, _ := job["state"].(string)
		if terminalPlanJob(job, stateText) {
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
	currentBefore := state.CurrentMilestone
	completedBefore := len(state.CompletedMilestones)
	advanceAppliedMilestone(root, state)
	if state.CurrentMilestone != currentBefore || len(state.CompletedMilestones) != completedBefore {
		changed = true
	}
	if !changed {
		return state, nil
	}
	state.Revision++
	if err := writePlans(path, states); err != nil {
		return nil, err
	}
	return state, nil
}

// ReconcilePlans reconciles every unfinished plan independently and returns
// the unfinished records in collection order.
func ReconcilePlans(root, repository string) ([]*PlanState, error) {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	states, err := readPlans(path)
	if err != nil {
		return nil, err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return nil, err
	}
	var unfinished []*PlanState
	changed := false
	for _, state := range states {
		if state.CurrentMilestone == 0 {
			continue
		}
		currentBefore := state.CurrentMilestone
		completedBefore := len(state.CompletedMilestones)
		if err := validatePlanArtifact(state); err != nil {
			return nil, err
		}
		linked := linkedJobs(state)
		remaining := make([]string, 0, len(linked))
		planChanged := false
		for _, jobID := range linked {
			data, readErr := os.ReadFile(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"))
			var job map[string]any
			if readErr == nil {
				readErr = json.Unmarshal(data, &job)
			}
			if readErr != nil || job["jobId"] != jobID {
				planChanged = true
				continue
			}
			taskID, _ := job["taskId"].(string)
			owner, milestone, matches, ownerErr := resolvePlanOwner(states, taskID)
			if ownerErr != nil {
				return nil, ownerErr
			}
			if taskID != "" && (!matches || owner != state) {
				return nil, fmt.Errorf("linked job %q does not belong uniquely to plan %q", jobID, state.TaskID)
			}
			if job["applyState"] == "applied" {
				if matches && owner == state && containsMilestone(state.AcceptedMilestones, milestone) && !hasAppliedJob(state, jobID) {
					state.AppliedPasses = append(state.AppliedPasses, PlanPass{Milestone: milestone, TaskID: taskID, JobID: jobID})
				}
				planChanged = true
				continue
			}
			stateText, _ := job["state"].(string)
			if terminalPlanJob(job, stateText) {
				planChanged = true
				continue
			}
			remaining = append(remaining, jobID)
		}
		setLinkedJobs(state, remaining)
		if state.PendingNextAction != pendingAction(state) {
			planChanged = true
		}
		state.PendingNextAction = pendingAction(state)
		advanceAppliedMilestone(root, state)
		if state.CurrentMilestone != currentBefore || len(state.CompletedMilestones) != completedBefore {
			planChanged = true
		}
		if planChanged {
			state.Revision++
			changed = true
		}
		if state.CurrentMilestone != 0 {
			unfinished = append(unfinished, state)
		}
	}
	if changed {
		if err := writePlans(path, states); err != nil {
			return nil, err
		}
	}
	return unfinished, nil
}

// PlanReadiness returns the ready passes under the existing plan lock for
// the plan that owns taskID.
func PlanReadiness(root, repository, taskID string) ([]ReadyMilestone, error) {
	path, canonical, err := planPath(root, repository)
	if err != nil {
		return nil, err
	}
	unlock, err := LockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	states, err := readPlans(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := checkPlanProjects(states, canonical); err != nil {
		return nil, err
	}
	state, _, matches, err := resolvePlanOwner(states, taskID)
	if err != nil || !matches {
		return nil, err
	}
	if err := validatePlanArtifact(state); err != nil {
		return nil, err
	}
	return readyMilestones(root, state), nil
}

func resolvePlanOwner(states []*PlanState, taskID string) (*PlanState, int, bool, error) {
	var owner *PlanState
	milestone := 0
	for _, state := range states {
		candidate, matches := planTaskMilestone(state.TaskID, taskID)
		if !matches {
			continue
		}
		if owner != nil {
			return nil, 0, false, fmt.Errorf("task id %q matches multiple plans", taskID)
		}
		owner, milestone = state, candidate
	}
	return owner, milestone, owner != nil, nil
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

func terminalPlanJob(job map[string]any, state string) bool {
	if discarded, _ := job["discarded"].(bool); discarded {
		return true
	}
	if state == "FAILED" || state == "CANCELLED" || state == "TIMED_OUT" {
		return true
	}
	if state != "SUCCEEDED" {
		return false
	}
	reviewed, _ := job["reviewedSnapshot"].(string)
	baseline, _ := job["baselineSnapshot"].(string)
	return reviewed != "" && reviewed == baseline
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
	if state.CurrentMilestone == 0 {
		return ""
	}
	if len(linkedJobs(state)) != 0 {
		return "apply"
	}
	return "dispatch"
}

func planDependenciesDone(state *PlanState, n int) bool {
	for _, m := range state.Milestones {
		if milestoneNumber(m.ID) != n {
			continue
		}
		for _, dep := range m.Dependencies {
			if !containsMilestone(state.CompletedMilestones, dep) {
				return false
			}
			for _, prerequisite := range state.Milestones {
				if milestoneNumber(prerequisite.ID) == dep && prerequisite.ValidationState != "" && prerequisite.ValidationState != "PASS" {
					return false
				}
			}
		}
		return true
	}
	return false
}

func appliedMilestoneAwaitingValidation(state *PlanState, n int) bool {
	if len(state.Milestones) == 0 {
		return false
	}
	for _, pass := range state.AppliedPasses {
		if pass.Milestone != n {
			continue
		}
		for _, milestone := range state.Milestones {
			if milestoneNumber(milestone.ID) == n {
				return milestone.ValidationState != "PASS"
			}
		}
	}
	return false
}

func readyMilestones(root string, state *PlanState) []ReadyMilestone {
	if state.UserStopped || state.Gate != "" {
		return nil
	}
	ready := []ReadyMilestone{}
	for _, n := range state.AcceptedMilestones {
		if len(state.Milestones) == 0 && n != state.CurrentMilestone {
			continue
		}
		if containsMilestone(state.CompletedMilestones, n) || len(state.Milestones) > 0 && !planDependenciesDone(state, n) {
			continue
		}
		if appliedMilestoneAwaitingValidation(state, n) {
			continue
		}
		// Any linked pass (including a result awaiting apply) prevents duplicate dispatch.
		linked := false
		for _, jobID := range linkedJobs(state) {
			job, ok := readSmallObject(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"))
			if !ok || job["jobId"] != jobID {
				linked = true
				break
			}
			task, _ := job["taskId"].(string)
			if milestone, matches := planTaskMilestone(state.TaskID, task); matches && milestone == n {
				linked = true
				break
			}
		}
		if linked {
			continue
		}
		// Recovery also sees running jobs whose link has not yet landed.
		if runningLinkedJob(root, state, n) != "" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, "codex_exec_mcp", "jobs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			job, ok := readSmallObject(filepath.Join(root, "codex_exec_mcp", "jobs", entry.Name()))
			if !ok {
				continue
			}
			task, _ := job["taskId"].(string)
			milestone, matches := planTaskMilestone(state.TaskID, task)
			if matches && milestone == n && job["discarded"] != true && (job["state"] == "QUEUED" || job["state"] == "RUNNING") {
				linked = true
				break
			}
		}
		if linked {
			continue
		}
		id := fmt.Sprintf("M%d", n)
		ready = append(ready, ReadyMilestone{ID: id, ArtifactRef: state.ArtifactPath + "#" + id})
	}
	return ready
}

func advanceAppliedMilestone(root string, state *PlanState) {
	for _, n := range state.AcceptedMilestones {
		if len(state.Milestones) == 0 && n != state.CurrentMilestone {
			continue
		}
		if containsMilestone(state.CompletedMilestones, n) || len(state.Milestones) > 0 && !planDependenciesDone(state, n) {
			continue
		}
		passed := false
		for _, pass := range state.AppliedPasses {
			if pass.Milestone == n && plainPlanTaskMilestone(state.TaskID, pass.TaskID, n) {
				passed = true
				break
			}
		}
		validationPassed := len(state.Milestones) == 0 || !appliedMilestoneAwaitingValidation(state, n)
		if passed && validationPassed && !hasOpenLinkedJob(root, state, n) {
			state.CompletedMilestones = append(state.CompletedMilestones, n)
			if len(state.Milestones) == 0 {
				break
			}
		}
	}
	state.CurrentMilestone = 0
	for _, n := range state.AcceptedMilestones {
		if !containsMilestone(state.CompletedMilestones, n) {
			state.CurrentMilestone = n
			break
		}
	}
	state.ReadyMilestones = readyMilestones(root, state)
	state.PendingNextAction = pendingAction(state)
}

func plainPlanTaskMilestone(planID, taskID string, milestone int) bool {
	if !strings.HasPrefix(taskID, planID+"-m") {
		return false
	}
	match := plainPlanTaskSuffix.FindStringSubmatch(strings.TrimPrefix(taskID, planID+"-m"))
	if match == nil {
		return false
	}
	value, err := strconv.Atoi(match[1])
	return err == nil && value == milestone
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
		if discarded, _ := job["discarded"].(bool); discarded {
			continue
		}
		if stateText == "SUCCEEDED" {
			reviewedSnapshot, _ := job["reviewedSnapshot"].(string)
			baselineSnapshot, _ := job["baselineSnapshot"].(string)
			if reviewedSnapshot != "" && reviewedSnapshot == baselineSnapshot {
				continue
			}
		}
		if stateText != "FAILED" && stateText != "CANCELLED" && stateText != "TIMED_OUT" {
			return true
		}
	}
	return false
}

func runningLinkedJob(root string, state *PlanState, milestone int) string {
	for _, jobID := range linkedJobs(state) {
		data, err := os.ReadFile(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"))
		if err != nil {
			continue
		}
		var job map[string]any
		if json.Unmarshal(data, &job) != nil || job["jobId"] != jobID {
			continue
		}
		if discarded, _ := job["discarded"].(bool); discarded {
			continue
		}
		taskID, _ := job["taskId"].(string)
		jobMilestone, matches := planTaskMilestone(state.TaskID, taskID)
		if !matches || jobMilestone != milestone {
			continue
		}
		stateText, _ := job["state"].(string)
		if stateText == "QUEUED" || stateText == "RUNNING" {
			return jobID
		}
	}
	return ""
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
