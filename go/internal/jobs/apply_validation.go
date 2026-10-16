package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/lease"
	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
	"purser-hook/internal/waiter"
)

const (
	validationOutputLimit = 1 << 20
	validationTimeout     = 5 * time.Minute
)

var launchApplyValidation = LaunchDetached
var runApplyValidationCommand = runValidationCommand
var validationLookPath = exec.LookPath
var applyValidationExecutable = resolveApplyValidationExecutable
var applyValidationTestLaunches atomic.Int64
var applyValidationTestCommands atomic.Int64

const applyValidationChildEnv = "PURSER_APPLY_VALIDATION_CHILD"

// DisableApplyValidationForTests prevents package tests from starting detached
// validators or running real validation commands.
func DisableApplyValidationForTests() {
	launchApplyValidation = func([]string, []string, string) (int, int64, error) {
		applyValidationTestLaunches.Add(1)
		return 0, 0, nil
	}
	runApplyValidationCommand = func(string, []string, []string, time.Duration, io.Writer, *applyValidation) error {
		applyValidationTestCommands.Add(1)
		return nil
	}
}

func resolveApplyValidationExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	if isGoTestExecutable(executable) {
		executable, err = exec.LookPath("purser-hook")
		if err != nil {
			return "", errors.New("purser-hook executable is unavailable")
		}
	}
	if isGoTestExecutable(executable) {
		return "", errors.New("refusing to launch a Go test executable for apply validation")
	}
	return executable, nil
}

func isGoTestExecutable(executable string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(executable)), ".test")
}

type applyValidation struct {
	JobID          string         `json:"job_id"`
	Repository     string         `json:"repository"`
	Revision       string         `json:"revision"`
	Snapshot       string         `json:"snapshot"`
	TestedSnapshot string         `json:"tested_snapshot,omitempty"`
	Paths          []string       `json:"paths"`
	Packages       []string       `json:"packages,omitempty"`
	Status         string         `json:"status"`
	Reason         string         `json:"reason,omitempty"`
	PluginStatus   string         `json:"plugin_test_status,omitempty"`
	PluginReason   string         `json:"plugin_test_reason,omitempty"`
	Commands       []string       `json:"commands,omitempty"`
	Counts         map[string]int `json:"counts,omitempty"`
	FailedTests    []string       `json:"failing_tests,omitempty"`
	UpdatedAt      string         `json:"updated_at"`
	PID            int            `json:"pid,omitempty"`
	Identity       int64          `json:"identity,omitempty"`
	TreeReceipt    string         `json:"tree_receipt,omitempty"`
}

// enqueueAppliedValidation is the single idempotent enqueue point for every successful apply outcome.
func enqueueAppliedValidation(stateRoot string, job map[string]any, value map[string]any) {
	if value["applied"] != true || os.Getenv(applyValidationChildEnv) == "1" {
		return
	}
	id, _ := job["jobId"].(string)
	repo, _ := job["repoRoot"].(string)
	paths := validationPaths(value["paths"])
	if id == "" || repo == "" {
		return
	}
	result := map[string]any{"status": "launch_error", "reason": "validation could not be enqueued"}
	defer func() {
		value["validation"] = result
		if status, ok := result["status"].(string); ok && (status == "pending" || status == "running" || status == "unconfirmed") {
			command := waiter.ValidationCommand(stateRoot, validationRepositoryKey(repo), id)
			value["waiter"] = command
			value["next"] = "Run " + command + " in the background (use the PowerShell tool on Windows); if it prints STILL_WAITING, run it again, otherwise use its final line to report host validation."
		}
		if result["status"] == "launch_error" {
			if _, err := readApplyValidation(validationRecordPath(stateRoot, validationRepositoryKey(repo), id)); err != nil {
				persistApplyValidationSummary(stateRoot, id, result)
			}
		}
		if status, ok := result["status"].(string); ok {
			_ = contextio.RecordPlanValidation(stateRoot, repo, stringValue(job["taskId"]), id,
				mapValidationState(status, stringValue(result["reason"])))
		}
	}()
	revision, snapshot, err := checkoutRevision(repo)
	if err != nil {
		result["reason"] = err.Error()
		return
	}
	key := validationRepositoryKey(repo)
	dir := filepath.Join(stateRoot, "apply_validation", key)
	if err = os.MkdirAll(dir, 0700); err != nil {
		result["reason"] = err.Error()
		return
	}
	path := filepath.Join(dir, id+".json")
	lock, err := lease.Lock(filepath.Join(dir, "enqueue.lock"))
	if err != nil {
		result["reason"] = err.Error()
		return
	}
	defer lock()
	if prior, e := readApplyValidation(path); e == nil {
		if prior.Status == "pending" || prior.Status == "running" || prior.Status == "unconfirmed" {
			result = validationSummary(prior)
			return
		}
		if prior.TreeReceipt != "" {
			if stopped, stopErr := treeStopped(filepath.Dir(path), prior.JobID, prior.TreeReceipt); !stopped {
				result = validationSummary(prior)
				result["status"], result["reason"] = "unconfirmed", stopErr.Error()
				return
			}
		}
	}
	invalidatePriorApplyValidations(stateRoot, dir, id, snapshot)
	if prior, readErr := readApplyValidation(path); readErr == nil && prior.Snapshot == snapshot {
		result = validationSummary(prior)
		return
	}
	record := applyValidation{JobID: id, Repository: repo, Revision: revision, Snapshot: snapshot, Paths: paths,
		Status: "pending", Counts: map[string]int{}, FailedTests: []string{}, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = writeApplyValidation(path, record); err != nil {
		result["reason"] = err.Error()
		return
	}
	_ = contextio.RecordPlanValidation(stateRoot, repo, stringValue(job["taskId"]), id, "PENDING")
	executable, err := applyValidationExecutable()
	if err != nil || isGoTestExecutable(executable) {
		if err == nil {
			err = errors.New("refusing to launch a Go test executable for apply validation")
		}
		record.Status = "launch_error"
		record.Reason = err.Error()
		_ = writeApplyValidation(path, record)
		result = validationSummary(record)
		return
	}
	env := setValidationChildEnv(os.Environ())
	pid, identity, err := launchApplyValidation([]string{executable, "validate-apply", stateRoot, key, id}, env, repo)
	if err != nil {
		record.Status = "launch_error"
		record.Reason = err.Error()
		record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_ = writeApplyValidation(path, record)
		result = validationSummary(record)
		_ = contextio.RecordPlanValidation(stateRoot, repo, stringValue(job["taskId"]), id, "LAUNCH_ERROR")
		return
	}
	record.PID, record.Identity = pid, identity
	_ = recordApplyValidationProcess(path, pid, identity)
	result = validationSummary(record)
}

// WaitApplyValidation observes the durable apply-validation record until it is
// terminal or the bounded wait expires. Read errors are retried because records
// are atomically replaced and Windows can briefly deny reads during replacement.
func WaitApplyValidation(stateRoot, key, id string) WaitResult {
	if !validJobID(id) || len(id) != 64 || len(key) != 64 {
		return WaitResult{Lines: []string{"purser-validation INVALID_ARGUMENT"}, ExitCode: 2}
	}
	if _, err := hex.DecodeString(key); err != nil {
		return WaitResult{Lines: []string{"purser-validation INVALID_ARGUMENT"}, ExitCode: 2}
	}
	started := time.Now()
	maxWaitSeconds := int64(9 * 60)
	if configured, err := strconv.ParseInt(os.Getenv("PURSER_WAIT_MAX_SECONDS"), 10, 64); err == nil && configured > 0 {
		maxWaitSeconds = configured
	}
	path := validationRecordPath(stateRoot, key, id)
	for {
		record, err := readApplyValidation(path)
		if err == nil && record.JobID == id && record.Status != "pending" && record.Status != "running" && record.Status != "unconfirmed" {
			line, _ := json.Marshal(map[string]any{"status": record.Status, "reason": record.Reason, "failing_tests": boundedValidationIdentifiers(record.FailedTests)})
			return WaitResult{Lines: []string{"purser-validation " + string(line)}, ExitCode: 0}
		}
		remaining := time.Duration(maxWaitSeconds)*time.Second - time.Since(started)
		if remaining <= 0 {
			return WaitResult{Lines: []string{"purser-validation STILL_WAITING"}, ExitCode: 5}
		}
		waitSleep(min(100*time.Millisecond, remaining))
	}
}

func persistApplyValidationSummary(stateRoot, id string, result map[string]any) {
	if stateRoot == "" || id == "" || result == nil {
		return
	}
	path := filepath.Join(stateRoot, "codex_exec_mcp", "jobs", id+".json")
	job, err := Read(path)
	if err != nil {
		return
	}
	if err = job.Set("applyValidation", result); err == nil {
		_ = Write(path, job)
	}
}

func invalidatePriorApplyValidations(stateRoot, dir, currentID, snapshot string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if id == currentID {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		record, err := readApplyValidation(path)
		if err != nil || record.Snapshot == snapshot || record.Status == "pending" || record.Status == "running" || record.Status == "unconfirmed" || record.Status == "interrupted" || record.Status == "stale" {
			continue
		}
		markApplyValidationStale(stateRoot, path, &record, "a later apply changed the repository checkout")
	}
}

func markApplyValidationStale(stateRoot, path string, record *applyValidation, reason string) {
	unlock, err := lease.Lock(path + ".lock")
	if err != nil {
		return
	}
	current, err := readApplyValidation(path)
	if err == nil && current.Status != "pending" && current.Status != "running" && current.Status != "unconfirmed" && current.Status != "stale" {
		current.Status, current.Reason = "stale", reason
		if writeApplyValidationUnlocked(path, current) == nil {
			*record = current
		} else {
			err = errors.New("validation record write failed")
		}
	}
	_ = unlock()
	if err != nil || record.Status != "stale" {
		return
	}
	jobPath := filepath.Join(stateRoot, "codex_exec_mcp", "jobs", record.JobID+".json")
	if job, e := Read(jobPath); e == nil {
		values := recordMap(job)
		_ = job.Set("applyValidation", validationSummary(*record))
		_ = Write(jobPath, job)
		_ = contextio.RecordPlanValidation(stateRoot, record.Repository, stringValue(values["taskId"]), record.JobID, "STALE")
	}
}

// ApplyValidationResult refreshes a durable result against the live checkout before returning it.
func ApplyValidationResult(stateRoot, repository, jobID string) map[string]any {
	path := validationRecordPath(stateRoot, validationRepositoryKey(repository), jobID)
	record, err := readApplyValidation(path)
	if err != nil {
		return nil
	}
	_, snapshot, err := checkoutRevision(repository)
	if err != nil {
		markApplyValidationStale(stateRoot, path, &record, "repository checkout could not be verified after validation")
		return validationSummary(record)
	}
	if snapshot != record.Snapshot && record.Status != "pending" && record.Status != "running" && record.Status != "unconfirmed" {
		markApplyValidationStale(stateRoot, path, &record, "repository checkout changed after validation")
	}
	return validationSummary(record)
}

func validationSummary(r applyValidation) map[string]any {
	value := map[string]any{"status": r.Status, "counts": r.Counts, "failing_tests": boundedValidationIdentifiers(r.FailedTests), "reason": r.Reason, "packages": boundedValidationIdentifiers(r.Packages)}
	if r.PluginStatus != "" {
		value["plugin_test"] = map[string]any{"status": r.PluginStatus, "reason": r.PluginReason}
	}
	return value
}

func boundedValidationIdentifiers(values []string) []string {
	const maxIdentifiers, maxLength = 100, 512
	if len(values) > maxIdentifiers {
		values = values[:maxIdentifiers]
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) > maxLength {
			value = value[:maxLength]
		}
		out = append(out, value)
	}
	return out
}

func validationPaths(value any) []string {
	switch paths := value.(type) {
	case []string:
		return append([]string(nil), paths...)
	case []any:
		out := make([]string, 0, len(paths))
		for _, path := range paths {
			if text, ok := path.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func validationRepositoryKey(repo string) string {
	root, _ := filepath.Abs(repo)
	h := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(root))))
	return hex.EncodeToString(h[:])
}

func validationRecordPath(root, key, id string) string {
	return filepath.Join(root, "apply_validation", key, id+".json")
}

func readApplyValidation(path string) (applyValidation, error) {
	var value applyValidation
	b, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	err = json.Unmarshal(b, &value)
	return value, err
}

func writeApplyValidation(path string, value applyValidation) error {
	unlock, err := lease.Lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	return writeApplyValidationUnlocked(path, value)
}

func writeApplyValidationUnlocked(path string, value applyValidation) error {
	value.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}

func recordApplyValidationProcess(path string, pid int, identity int64) error {
	unlock, err := lease.Lock(path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	record, err := readApplyValidation(path)
	if err != nil {
		return err
	}
	if record.Status != "pending" && record.Status != "running" {
		return nil
	}
	record.PID, record.Identity = pid, identity
	return writeApplyValidationUnlocked(path, record)
}

func checkoutRevision(repo string) (string, string, error) {
	cmd := proc.Hide(exec.Command("git", "rev-parse", "HEAD"))
	cmd.Dir = repo
	head, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	revision := string(bytes.TrimSpace(head))
	cmd = proc.Hide(exec.Command("git", "rev-parse", "--git-path", "index"))
	cmd.Dir = repo
	index, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	indexPath := string(bytes.TrimSpace(index))
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repo, indexPath)
	}
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return "", "", err
	}
	tempIndex, err := os.CreateTemp(filepath.Dir(indexPath), "purser-validation-index-")
	if err != nil {
		return "", "", err
	}
	tempPath := tempIndex.Name()
	if err = tempIndex.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", "", err
	}
	defer os.Remove(tempPath)
	if err = os.WriteFile(tempPath, indexData, 0600); err != nil {
		return "", "", err
	}
	env := replaceValidationEnv(os.Environ(), "GIT_INDEX_FILE", tempPath)
	cmd = proc.Hide(exec.Command("git", "add", "--renormalize", "-A"))
	cmd.Dir, cmd.Env = repo, env
	if err = cmd.Run(); err != nil {
		return "", "", err
	}
	cmd = proc.Hide(exec.Command("git", "write-tree"))
	cmd.Dir, cmd.Env = repo, env
	tree, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	snapshot := strings.TrimSpace(string(tree))
	return revision, snapshot, nil
}

func replaceValidationEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), prefix) {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}

func validationPackageSnapshot(repo string, packages []string) (string, error) {
	roots := map[string]struct{}{}
	for _, spec := range packages {
		parts := strings.SplitN(spec, "|", 2)
		if len(parts) != 2 {
			continue
		}
		root := parts[0]
		pkg := parts[1]
		if pkg == "." || strings.HasPrefix(pkg, "./") {
			root = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, "./")))
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			return "", err
		}
		relative, err := filepath.Rel(repo, absolute)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return "", errors.New("validation package escapes repository")
		}
		roots[filepath.ToSlash(filepath.Clean(relative))] = struct{}{}
	}
	ordered := make([]string, 0, len(roots))
	for root := range roots {
		ordered = append(ordered, root)
	}
	sort.Strings(ordered)
	h := sha256.New()
	for _, root := range ordered {
		cmd := proc.Hide(exec.Command("git", "-C", repo, "ls-files", "-z", "--", root))
		files, err := cmd.Output()
		if err != nil {
			return "", err
		}
		for _, raw := range bytes.Split(files, []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			name := filepath.FromSlash(string(raw))
			data, readErr := os.ReadFile(filepath.Join(repo, name))
			_, _ = h.Write(raw)
			_, _ = h.Write([]byte{0})
			if readErr == nil {
				_, _ = h.Write(data)
			} else if errors.Is(readErr, os.ErrNotExist) {
				_, _ = h.Write([]byte("<deleted>"))
			} else {
				return "", readErr
			}
			_, _ = h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RunApplyValidation is the private detached worker entry point.
func RunApplyValidation(stateRoot, key, id string) error {
	if !validJobID(id) || len(id) != 64 || len(key) != 64 {
		return errors.New("invalid apply validation identity")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return errors.New("invalid apply validation repository key")
	}
	path := validationRecordPath(stateRoot, key, id)
	record, err := readApplyValidation(path)
	if err != nil {
		return err
	}
	if record.JobID != id || record.Status != "pending" {
		return errors.New("validation is not pending")
	}
	lock, err := contextio.LockFileBlocking(filepath.Join(stateRoot, "apply_validation", key, "repository.lock"))
	if err != nil {
		return err
	}
	defer lock()
	cacheUser, err := acquireCacheUser(stateRoot)
	if err != nil {
		return err
	}
	defer cacheUser()
	record.Status = "running"
	if err = writeApplyValidation(path, record); err != nil {
		return err
	}
	_, snapshot, err := checkoutRevision(record.Repository)
	if err != nil {
		return finishValidation(stateRoot, path, &record, "launch_error", err.Error())
	}
	if snapshot != record.Snapshot {
		return finishValidation(stateRoot, path, &record, "stale", "checkout changed before validation started")
	}
	goRoot, modules, packages, err := selectValidationPackages(stateRoot, record.Repository, record.Paths)
	validationState, validationReason := "", ""
	if err != nil {
		validationState, validationReason = validationFailureState(err), err.Error()
	}
	record.Packages = boundedValidationIdentifiers(packages)
	if len(packages) > 0 {
		record.TestedSnapshot, err = validationPackageSnapshot(record.Repository, record.Packages)
		if err != nil {
			return finishValidation(stateRoot, path, &record, "stale", "validated checkout snapshot could not be recorded")
		}
		if err = writeApplyValidation(path, record); err != nil {
			return err
		}
	}
	profileConfig := profile.LoadProfile(record.Repository)
	customCommand := ""
	if profileConfig.ValidationCommand != nil {
		customCommand = *profileConfig.ValidationCommand
	}
	ecosystemCommands := []ecosystemValidationCommand{}
	if customCommand == "" {
		ecosystemCommands = selectEcosystemValidationCommands(record.Repository, record.Paths)
	}
	if len(packages) == 0 && !touchesPlugin(record.Paths) && customCommand == "" && len(ecosystemCommands) == 0 && validationState == "" {
		return finishValidation(stateRoot, path, &record, "skipped", "no Go packages or plugin paths were changed")
	}
	logPath := filepath.Join(filepath.Dir(path), id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return finishValidation(stateRoot, path, &record, "launch_error", err.Error())
	}
	defer logFile.Close()
	for _, module := range modules {
		env := moduleValidationEnvironment(stateRoot, module)
		if len(packages) > 0 {
			pkgs := packagesForModule(packages, goRoot, module)
			if len(pkgs) > 0 {
				args := append([]string{"vet", "-p", "2"}, pkgs...)
				record.Commands = appendValidationCommand(record.Commands, "go "+strings.Join(args, " "))
				if err = runApplyValidationCommand(module, env, args, validationTimeout, logFile, nil); err != nil {
					if errors.Is(err, errTreeUnconfirmed) {
						return finishValidation(stateRoot, path, &record, "unconfirmed", err.Error())
					}
					setValidationOutcome(&validationState, &validationReason, validationFailureState(err), "go vet failed")
				}
			}
		}
	}
	if len(packages) > 0 {
		for _, module := range modules {
			env := moduleValidationEnvironment(stateRoot, module)
			args := append([]string{"test", "-p", "2", "-json"}, packagesForModule(packages, goRoot, module)...)
			record.Commands = appendValidationCommand(record.Commands, "go "+strings.Join(args, " "))
			if err = runApplyValidationCommand(module, env, args, validationTimeout, logFile, &record); err != nil {
				if errors.Is(err, errTreeUnconfirmed) {
					return finishValidation(stateRoot, path, &record, "unconfirmed", err.Error())
				}
				setValidationOutcome(&validationState, &validationReason, validationFailureState(err), "go test failed")
			}
		}
	}
	if customCommand != "" {
		shell, shellArgs, tool := validationShell(customCommand)
		if binary, lookErr := validationLookPath(shell); lookErr != nil {
			setValidationOutcome(&validationState, &validationReason, "skipped", tool+" is unavailable")
		} else {
			args := append([]string{binary}, shellArgs...)
			record.Commands = appendValidationCommand(record.Commands, tool+" -c "+customCommand)
			if err = runApplyValidationCommand(record.Repository, validationEnvironment(stateRoot), args, validationTimeout, logFile, nil); err != nil {
				if errors.Is(err, errTreeUnconfirmed) {
					return finishValidation(stateRoot, path, &record, "unconfirmed", err.Error())
				}
				setValidationOutcome(&validationState, &validationReason, validationFailureState(err), "validation command failed")
			}
		}
	} else {
		for _, check := range ecosystemCommands {
			binary, lookErr := validationLookPath(check.tool)
			if lookErr != nil && check.tool == "python" {
				check.tool = "python3"
				binary, lookErr = validationLookPath(check.tool)
			}
			if lookErr != nil {
				setValidationOutcome(&validationState, &validationReason, "skipped", check.tool+" is unavailable")
				continue
			}
			args := []string{binary}
			args = append(args, check.args...)
			record.Commands = appendValidationCommand(record.Commands, check.tool+" "+strings.Join(check.args, " "))
			if err = runApplyValidationCommand(check.dir, validationEnvironment(stateRoot), args, validationTimeout, logFile, nil); err != nil {
				if errors.Is(err, errTreeUnconfirmed) {
					return finishValidation(stateRoot, path, &record, "unconfirmed", err.Error())
				}
				setValidationOutcome(&validationState, &validationReason, validationFailureState(err), check.label+" failed")
			}
		}
	}
	if touchesPlugin(record.Paths) {
		env := validationEnvironment(stateRoot)
		claude, lookErr := validationLookPath("claude")
		if lookErr != nil {
			record.PluginStatus, record.PluginReason = "skipped", "claude binary is unavailable"
			setValidationOutcome(&validationState, &validationReason, "skipped", record.PluginReason)
		} else {
			files, listErr := trackedPluginTestFiles(record.Repository)
			if listErr != nil {
				record.PluginStatus, record.PluginReason = validationFailureState(listErr), "claude plugin test failed"
				setValidationOutcome(&validationState, &validationReason, record.PluginStatus, record.PluginReason)
			} else if len(files) == 0 {
				record.PluginStatus, record.PluginReason = "skipped", "no tracked plugin test files"
				setValidationOutcome(&validationState, &validationReason, "skipped", record.PluginReason)
			} else {
				record.PluginStatus = "passed"
				for _, file := range files {
					args := []string{claude, "plugin", "test", "--file", file, "."}
					record.Commands = appendValidationCommand(record.Commands, "claude plugin test --file "+file)
					if err = runApplyValidationCommand(record.Repository, env, args, validationTimeout, logFile, nil); err != nil {
						if errors.Is(err, errTreeUnconfirmed) {
							return finishValidation(stateRoot, path, &record, "unconfirmed", err.Error())
						}
						record.PluginStatus, record.PluginReason = validationFailureState(err), "claude plugin test failed"
						setValidationOutcome(&validationState, &validationReason, record.PluginStatus, record.PluginReason)
					}
				}
			}
		}
	}
	if validationState != "" {
		return finishValidation(stateRoot, path, &record, validationState, validationReason)
	}
	return finishValidation(stateRoot, path, &record, "passed", "")
}

func validationFailureState(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) || errors.Is(err, os.ErrNotExist) {
		return "launch_error"
	}
	return "failed"
}

// RecoverApplyValidations marks detached checks left without a live owner as interrupted.
func RecoverApplyValidations(stateRoot string) {
	base := filepath.Join(stateRoot, "apply_validation")
	keys, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range keys {
		if entry.IsDir() {
			dir := filepath.Join(base, entry.Name())
			unlock, lockErr := lease.Lock(filepath.Join(dir, "enqueue.lock"))
			if lockErr != nil {
				continue
			}
			recoverApplyValidationsInDirectory(stateRoot, dir)
			_ = unlock()
		}
	}
}

func recoverApplyValidationsInDirectory(stateRoot, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == "enqueue.lock" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		unlock, lockErr := lease.Lock(path + ".lock")
		if lockErr != nil {
			continue
		}
		record, readErr := readApplyValidation(path)
		if readErr != nil || record.Status != "pending" && record.Status != "running" && record.Status != "unconfirmed" || record.PID > 0 && record.Identity > 0 && recoveryProcessState(record.PID, uint64(record.Identity)) != "DEAD" {
			_ = unlock()
			continue
		}
		if record.TreeReceipt != "" {
			if stopped, stopErr := treeStopped(filepath.Dir(path), record.JobID, record.TreeReceipt); !stopped {
				record.Status, record.Reason = "unconfirmed", stopErr.Error()
				_ = writeApplyValidationUnlocked(path, record)
				_ = unlock()
				continue
			}
		}
		record.Status, record.Reason = "interrupted", "validation process no longer matches its recorded identity"
		writeErr := writeApplyValidationUnlocked(path, record)
		_ = unlock()
		if writeErr != nil {
			continue
		}
		jobPath := filepath.Join(stateRoot, "codex_exec_mcp", "jobs", record.JobID+".json")
		if job, e := Read(jobPath); e == nil {
			task := stringValue(recordMap(job)["taskId"])
			_ = job.Set("applyValidation", validationSummary(record))
			_ = Write(jobPath, job)
			_ = contextio.RecordPlanValidation(stateRoot, record.Repository, task, record.JobID, "INTERRUPTED")
		}
	}
}

func finishValidation(root, path string, record *applyValidation, status, reason string) error {
	_, currentSnapshot, err := checkoutRevision(record.Repository)
	packageSnapshot := ""
	if err == nil && record.TestedSnapshot != "" {
		packageSnapshot, err = validationPackageSnapshot(record.Repository, record.Packages)
	}
	if err == nil && status != "launch_error" && status != "unconfirmed" && (currentSnapshot != record.Snapshot || packageSnapshot != record.TestedSnapshot) {
		status = "stale"
		reason = "checkout changed during validation"
	} else if err != nil && status == "passed" {
		status = "stale"
		reason = "checkout snapshot could not be verified after validation"
	}
	if current, readErr := readApplyValidation(path); readErr == nil {
		record.TreeReceipt = current.TreeReceipt
	}
	record.Status, record.Reason = status, reason
	if err = writeApplyValidation(path, *record); err != nil {
		return err
	}
	jobPath := filepath.Join(root, "codex_exec_mcp", "jobs", record.JobID+".json")
	if job, e := Read(jobPath); e == nil {
		values := recordMap(job)
		_ = job.Set("applyValidation", validationSummary(*record))
		_ = Write(jobPath, job)
		_ = contextio.RecordPlanValidation(root, record.Repository, stringValue(values["taskId"]), record.JobID, mapValidationState(status, reason))
	}
	if status == "failed" || status == "timeout" {
		summarizeApplyValidationFailure(record, strings.TrimSuffix(path, ".json")+".log")
	}
	return nil
}

func mapValidationState(status, reason string) string {
	if status == "passed" {
		return "PASS"
	}
	return strings.ToUpper(status)
}

// summarizeApplyValidationFailure is the hook for the separately owned failure summarizer.
func summarizeApplyValidationFailure(_ *applyValidation, _ string) {}

type limitedCapture struct {
	file  io.Writer
	limit int64
	kept  int64
}

func (w *limitedCapture) Write(p []byte) (int, error) {
	n := len(p)
	if w.kept < w.limit {
		take := int64(n)
		if take > w.limit-w.kept {
			take = w.limit - w.kept
		}
		written, err := w.file.Write(p[:take])
		w.kept += int64(written)
		if err != nil {
			return written, err
		}
	}
	return n, nil
}

func runValidationCommand(dir string, env, args []string, timeout time.Duration, log io.Writer, record *applyValidation) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	bin := "go"
	if len(args) > 0 && filepath.IsAbs(args[0]) {
		bin, args = args[0], args[1:]
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	proc.ConfigureProcessTreeKill(cmd)
	proc.ConfigureBelowNormalPriority(cmd)
	capture := &limitedCapture{file: log, limit: validationOutputLimit}
	if file, ok := log.(*os.File); ok {
		if info, statErr := file.Stat(); statErr == nil {
			capture.kept = info.Size()
		}
	}
	parsed := &boundedValidationBuffer{}
	if record != nil {
		cmd.Stdout = io.MultiWriter(capture, parsed)
	} else {
		cmd.Stdout = capture
	}
	cmd.Stderr = capture
	receiptPath := ""
	if file, ok := log.(*os.File); ok {
		validationPath := strings.TrimSuffix(file.Name(), ".log") + ".json"
		current, readErr := readApplyValidation(validationPath)
		if readErr == nil {
			prepared, prepareErr := prepareTreeReceipt(filepath.Dir(validationPath), current.JobID, current.TreeReceipt)
			if prepareErr != nil {
				return prepareErr
			}
			receiptPath = prepared
			current.TreeReceipt = receiptPath
			if err := writeApplyValidation(validationPath, current); err != nil {
				return err
			}
			if record != nil {
				record.TreeReceipt = receiptPath
			}
		}
	}
	supervised, err := startSupervisedCommand(ctx, cmd, receiptPath)
	if err != nil {
		if receiptPath != "" {
			err = errors.Join(err, writeTreeReceipt(receiptPath, true, err))
		}
		return err
	}
	r, err := supervised.wait()
	if !r.Confirmed {
		return errors.Join(err, errTreeUnconfirmed)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if record != nil {
		parseTestJSON(bytes.NewBuffer(parsed.data), record)
	}
	return err
}

func setValidationChildEnv(env []string) []string {
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, applyValidationChildEnv) {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, applyValidationChildEnv+"=1")
}

type boundedValidationBuffer struct{ data []byte }

func (b *boundedValidationBuffer) Write(value []byte) (int, error) {
	n := len(value)
	remaining := validationOutputLimit - len(b.data)
	if remaining > 0 {
		if n > remaining {
			n = remaining
		}
		b.data = append(b.data, value[:n]...)
	}
	return len(value), nil
}

func parseTestJSON(data *bytes.Buffer, record *applyValidation) {
	if record.Counts == nil {
		record.Counts = map[string]int{}
	}
	dec := json.NewDecoder(bytes.NewReader(data.Bytes()))
	for {
		var event struct {
			Action string
			Test   string
		}
		if dec.Decode(&event) != nil {
			break
		}
		if event.Test != "" {
			switch event.Action {
			case "pass":
				record.Counts["tests_passed"]++
			case "fail":
				record.Counts["tests_failed"]++
				if len(record.FailedTests) < 100 {
					record.FailedTests = append(record.FailedTests, event.Test)
				}
			case "skip":
				record.Counts["tests_skipped"]++
			}
		} else if event.Action == "pass" {
			record.Counts["packages_passed"]++
		} else if event.Action == "fail" {
			record.Counts["packages_failed"]++
		}
	}
	sort.Strings(record.FailedTests)
}

func validationEnvironment(root string) []string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		if i := strings.IndexByte(entry, '='); i > 0 {
			values[strings.ToUpper(entry[:i])] = entry[i+1:]
		}
	}
	values["GOTELEMETRY"] = "off"
	values["GOCACHE"] = filepath.Join(root, "cache", "go-build")
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values[k])
	}
	return out
}

func moduleValidationEnvironment(root, module string) []string {
	env := validationEnvironment(root)
	if _, err := os.Stat(filepath.Join(module, "go.mod")); errors.Is(err, os.ErrNotExist) {
		for i, entry := range env {
			if strings.HasPrefix(strings.ToUpper(entry), "GO111MODULE=") {
				env[i] = "GO111MODULE=off"
				return env
			}
		}
		env = append(env, "GO111MODULE=off")
	}
	return env
}

type ecosystemValidationCommand struct {
	label string
	tool  string
	dir   string
	args  []string
}

func validationShell(command string) (string, []string, string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}, "cmd"
	}
	return "sh", []string{"-c", command}, "sh"
}

func selectEcosystemValidationCommands(root string, paths []string) []ecosystemValidationCommand {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	selected := map[string]ecosystemValidationCommand{}
	for _, name := range paths {
		clean := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(clean))
		base := strings.ToLower(filepath.Base(clean))
		if ext == ".py" && selected["python"].tool == "" {
			if dir, ok := nearestValidationManifest(root, filepath.Dir(clean), []string{"pyproject.toml", "setup.cfg", "pytest.ini", "tox.ini"}); ok {
				selected["python"] = ecosystemValidationCommand{label: "python -m pytest -q", tool: "python", dir: dir, args: []string{"-m", "pytest", "-q"}}
			}
		}
		if (ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx" || ext == ".mjs" || ext == ".cjs") && selected["javascript"].tool == "" {
			if dir, ok := nearestValidationManifest(root, filepath.Dir(clean), []string{"package.json"}); ok && hasNpmTest(filepath.Join(dir, "package.json")) {
				selected["javascript"] = ecosystemValidationCommand{label: "npm test --silent", tool: "npm", dir: dir, args: []string{"test", "--silent"}}
			}
		}
		if (ext == ".rs" || base == "cargo.toml") && selected["rust"].tool == "" {
			start := filepath.Dir(clean)
			if base == "cargo.toml" {
				start = filepath.Dir(clean)
			}
			if dir, ok := nearestValidationManifest(root, start, []string{"Cargo.toml"}); ok {
				selected["rust"] = ecosystemValidationCommand{label: "cargo test", tool: "cargo", dir: dir, args: []string{"test"}}
			}
		}
	}
	commands := make([]ecosystemValidationCommand, 0, len(selected))
	for _, ecosystem := range []string{"python", "javascript", "rust"} {
		if command, ok := selected[ecosystem]; ok {
			commands = append(commands, command)
		}
	}
	return commands
}

func nearestValidationManifest(root, relativeDir string, names []string) (string, bool) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	dir := filepath.Join(root, relativeDir)
	for {
		for _, name := range names {
			if info, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil && !info.IsDir() {
				return dir, true
			}
		}
		if filepath.Clean(dir) == filepath.Clean(root) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

func hasNpmTest(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return false
	}
	test, ok := manifest.Scripts["test"]
	return ok && strings.TrimSpace(test) != "" && strings.TrimSpace(test) != `echo "Error: no test specified" && exit 1`
}

func appendValidationCommand(commands []string, command string) []string {
	if len(commands) >= 20 {
		return commands
	}
	if len(command) > 512 {
		command = command[:512]
	}
	return append(commands, command)
}

func setValidationOutcome(status, reason *string, candidate, candidateReason string) {
	rank := func(value string) int {
		switch value {
		case "timeout":
			return 4
		case "failed", "launch_error":
			return 3
		case "skipped":
			return 1
		default:
			return 0
		}
	}
	if rank(candidate) > rank(*status) {
		*status, *reason = candidate, candidateReason
	}
}

func selectValidationPackages(stateRoot, repo string, paths []string) (string, []string, []string, error) {
	modules := map[string]bool{}
	dirs := map[string]bool{}
	for _, name := range paths {
		clean := filepath.Clean(filepath.FromSlash(name))
		full := filepath.Join(repo, clean)
		if filepath.Base(clean) == "go.mod" || filepath.Base(clean) == "go.work" {
			modules[filepath.Dir(full)] = true
			continue
		}
		if filepath.Ext(clean) != ".go" {
			continue
		}
		dir := filepath.Dir(full)
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				modules[dir] = true
				break
			}
			if dir == repo {
				modules[repo] = true
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				modules[repo] = true
				break
			}
			dir = parent
		}
		pkg := filepath.Dir(full)
		for {
			if info, err := os.Stat(pkg); err == nil && info.IsDir() {
				break
			}
			parent := filepath.Dir(pkg)
			if parent == pkg {
				break
			}
			pkg = parent
		}
		dirs[pkg] = true
	}
	if len(modules) == 0 && len(dirs) > 0 {
		modules[repo] = true
	}
	mods := make([]string, 0, len(modules))
	for m := range modules {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	packages := []string{}
	for _, mod := range mods {
		moduleTouched := false
		for _, p := range paths {
			if filepath.Dir(filepath.Join(repo, filepath.FromSlash(p))) == mod && (filepath.Base(p) == "go.mod" || filepath.Base(p) == "go.work") {
				moduleTouched = true
			}
		}
		if moduleTouched {
			if _, statErr := os.Stat(filepath.Join(mod, "go.mod")); errors.Is(statErr, os.ErrNotExist) {
				found, walkErr := goPackageDirectories(mod)
				if walkErr != nil {
					return "", nil, nil, walkErr
				}
				for _, dir := range found {
					rel, _ := filepath.Rel(mod, dir)
					pkg := "./" + filepath.ToSlash(rel)
					if rel == "." {
						pkg = "."
					}
					packages = append(packages, mod+"|"+pkg)
				}
			} else {
				cmd := proc.Hide(exec.Command("go", "list", "./..."))
				cmd.Dir = mod
				cmd.Env = validationEnvironment(stateRoot)
				out, err := cmd.Output()
				if err != nil {
					return "", nil, nil, err
				}
				for _, line := range strings.Fields(string(out)) {
					packages = append(packages, mod+"|"+line)
				}
			}
			continue
		}
		for dir := range dirs {
			if dir == mod || strings.HasPrefix(dir, mod+string(os.PathSeparator)) {
				rel, err := filepath.Rel(mod, dir)
				if err != nil {
					continue
				}
				if _, err = os.Stat(filepath.Join(dir, "go.mod")); err == nil && dir != mod {
					continue
				}
				importPath := "./" + filepath.ToSlash(rel)
				if rel == "." {
					importPath = "."
				}
				packages = append(packages, mod+"|"+importPath)
			}
		}
	}
	sort.Strings(packages)
	packages = uniqueStrings(packages)
	bare := make([]string, 0, len(packages))
	roots := make([]string, 0, len(mods))
	for _, m := range mods {
		roots = append(roots, m)
	}
	for _, p := range packages {
		parts := strings.SplitN(p, "|", 2)
		bare = append(bare, p)
		_ = parts
	}
	return repo, roots, bare, nil
}

func goPackageDirectories(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			if name := entry.Name(); name == ".git" || name == "vendor" || name == ".tmp" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".go" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return uniqueStrings(dirs), nil
}

func packagesForModule(packages []string, root, module string) []string {
	out := []string{}
	for _, p := range packages {
		parts := strings.SplitN(p, "|", 2)
		if len(parts) == 2 && filepath.Clean(parts[0]) == filepath.Clean(module) {
			out = append(out, parts[1])
		}
	}
	return out
}
func uniqueStrings(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
func touchesPlugin(paths []string) bool {
	for _, p := range paths {
		p = filepath.ToSlash(p)
		if strings.HasPrefix(p, "hooks/") || strings.Contains(p, "/hooks/") || strings.Contains(p, "mod/") || strings.HasPrefix(p, "mod/") {
			return true
		}
	}
	return false
}

func trackedPluginTestFiles(repo string) ([]string, error) {
	command := proc.Hide(exec.Command("git", "ls-files", "-z", "--", "*.test.ts", "*.test.tsx"))
	command.Dir = repo
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	if len(output) == 0 {
		return nil, nil
	}
	files := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	sort.Strings(files)
	return files, nil
}
