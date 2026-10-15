package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func validationFixture(t *testing.T, paths []string) (string, string, applyValidation) {
	t.Helper()
	base := jobsScratchDir(t, "apply-validation-")
	repo := filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "go.mod"), "module example.test/validation\n\ngo 1.23\n")
	write(t, filepath.Join(repo, "main.go"), "package validation\nfunc Value() int { return 1 }\n")
	git(t, repo, "add", "--all")
	git(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "base")
	write(t, filepath.Join(repo, "main.go"), "package validation\nfunc Value() int { return 2 }\n")
	revision, snapshot, err := checkoutRevision(repo)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "state"), repo, applyValidation{JobID: strings.Repeat("a", 64), Repository: repo, Revision: revision, Snapshot: snapshot, Paths: paths, Status: "pending"}
}

func runValidationFixture(t *testing.T, status string, commandErr error) applyValidation {
	t.Helper()
	root, _, record := validationFixture(t, []string{"main.go"})
	key := validationRepositoryKey(record.Repository)
	path := validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	prior := runApplyValidationCommand
	runApplyValidationCommand = func(_ string, _ []string, args []string, _ time.Duration, _ io.Writer, result *applyValidation) error {
		if len(args) > 0 && args[0] == "test" && result != nil {
			if commandErr != nil {
				result.Counts = map[string]int{"tests_failed": 1}
				result.FailedTests = []string{"TestBad"}
			} else {
				result.Counts = map[string]int{"tests_passed": 1}
			}
			return commandErr
		}
		if errors.Is(commandErr, context.DeadlineExceeded) {
			return commandErr
		}
		return nil
	}
	t.Cleanup(func() { runApplyValidationCommand = prior })
	if err := RunApplyValidation(root, key, record.JobID); err != nil {
		t.Fatal(err)
	}
	got, err := readApplyValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != status {
		t.Fatalf("status=%q want %q (%s)", got.Status, status, got.Reason)
	}
	return got
}

func TestValidationPass(t *testing.T) {
	got := runValidationFixture(t, "passed", nil)
	if got.Counts["tests_passed"] != 1 {
		t.Fatalf("counts missing: %#v", got.Counts)
	}
}

func TestValidationPackageSelectionIncludesDeletedGoAndTouchedModule(t *testing.T) {
	t.Setenv("GOWORK", "off")
	root, repo, _ := validationFixture(t, []string{"main.go"})
	if err := os.Remove(filepath.Join(repo, "main.go")); err != nil {
		t.Fatal(err)
	}
	_, modules, packages, err := selectValidationPackages(root, repo, []string{"main.go"})
	if err != nil || len(modules) != 1 || len(packages) != 1 || packages[0] != repo+"|." {
		t.Fatalf("deleted Go path selection: modules=%v packages=%v err=%v", modules, packages, err)
	}
	write(t, filepath.Join(repo, "main.go"), "package validation\nfunc Value() int { return 3 }\n")
	_, modules, packages, err = selectValidationPackages(root, repo, []string{"go.mod"})
	if err != nil || len(modules) != 1 || len(packages) == 0 {
		t.Fatalf("module configuration selection: modules=%v packages=%v err=%v", modules, packages, err)
	}
}

func TestValidationPythonManifestDirectory(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"pkg/tests/test_one.py"})
	write(t, filepath.Join(repo, "pkg", "pyproject.toml"), "[tool.pytest.ini_options]\n")
	write(t, filepath.Join(repo, "pkg", "tests", "test_one.py"), "")
	commands := selectEcosystemValidationCommands(repo, record.Paths)
	if len(commands) != 1 || commands[0].tool != "python" || commands[0].dir != filepath.Join(repo, "pkg") || strings.Join(commands[0].args, " ") != "-m pytest -q" {
		t.Fatalf("Python validation selection=%+v (root %s)", commands, root)
	}
}

func TestValidationNpmPlaceholderIsNotRun(t *testing.T) {
	_, repo, record := validationFixture(t, []string{"web/test.ts"})
	write(t, filepath.Join(repo, "web", "package.json"), `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`)
	write(t, filepath.Join(repo, "web", "test.ts"), "")
	if got := selectEcosystemValidationCommands(repo, record.Paths); len(got) != 0 {
		t.Fatalf("placeholder npm test was selected: %+v", got)
	}
}

func TestValidationProfileCommandOverridesEcosystemDetection(t *testing.T) {
	got := runEcosystemValidationFixture(t, []string{"test.py"}, func(repo string) {
		write(t, filepath.Join(repo, "pyproject.toml"), "")
		write(t, filepath.Join(repo, "test.py"), "")
		write(t, filepath.Join(repo, ".orchestrator", "profile.toml"), "schema_version = 1\n[validation]\ncommand = \"custom-check --all\"\n")
	}, func(tool string) (string, error) { return "/fake/" + tool, nil }, func(dir string, _ []string, args []string, _ time.Duration, _ io.Writer, _ *applyValidation) error {
		if dir == "" || len(args) != 3 || args[len(args)-1] != "custom-check --all" {
			t.Fatalf("profile command invocation dir=%q args=%v", dir, args)
		}
		return nil
	})
	if got.Status != "passed" || len(got.Commands) != 1 || !strings.Contains(got.Commands[0], "custom-check --all") {
		t.Fatalf("profile command result=%+v", got)
	}
}

func TestValidationMissingEcosystemToolIsSkipped(t *testing.T) {
	got := runEcosystemValidationFixture(t, []string{"test.py"}, func(repo string) {
		write(t, filepath.Join(repo, "pyproject.toml"), "")
		write(t, filepath.Join(repo, "test.py"), "")
	}, func(string) (string, error) { return "", exec.ErrNotFound }, func(string, []string, []string, time.Duration, io.Writer, *applyValidation) error {
		t.Fatal("runner called for missing pytest executable")
		return nil
	})
	if got.Status != "skipped" || got.Reason != "python3 is unavailable" && got.Reason != "python is unavailable" {
		t.Fatalf("missing tool result=%+v", got)
	}
}

func TestValidationFailingEcosystemCommandFails(t *testing.T) {
	got := runEcosystemValidationFixture(t, []string{"test.py"}, func(repo string) {
		write(t, filepath.Join(repo, "pyproject.toml"), "")
		write(t, filepath.Join(repo, "test.py"), "")
	}, func(tool string) (string, error) { return "/fake/" + tool, nil }, func(string, []string, []string, time.Duration, io.Writer, *applyValidation) error {
		return errors.New("exit status 1")
	})
	if got.Status != "failed" || got.Reason != "python -m pytest -q failed" {
		t.Fatalf("failing ecosystem result=%+v", got)
	}
}

func runEcosystemValidationFixture(t *testing.T, paths []string, setup func(string), lookPath func(string) (string, error), runner func(string, []string, []string, time.Duration, io.Writer, *applyValidation) error) applyValidation {
	t.Helper()
	root, repo, record := validationFixture(t, paths)
	setup(repo)
	revision, snapshot, err := checkoutRevision(repo)
	if err != nil {
		t.Fatal(err)
	}
	record.Revision, record.Snapshot = revision, snapshot
	key := validationRepositoryKey(repo)
	path := validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	priorRunner, priorLookPath := runApplyValidationCommand, validationLookPath
	runApplyValidationCommand, validationLookPath = runner, lookPath
	t.Cleanup(func() { runApplyValidationCommand, validationLookPath = priorRunner, priorLookPath })
	if err := RunApplyValidation(root, key, record.JobID); err != nil {
		t.Fatal(err)
	}
	result, err := readApplyValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCheckoutRevisionFailsWhenDirectoryReplacesTrackedFile(t *testing.T) {
	_, repo, _ := validationFixture(t, []string{"main.go"})
	if err := os.Remove(filepath.Join(repo, "main.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, "main.go"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkoutRevision(repo); err == nil {
		t.Fatal("directory replacing tracked file produced an unverifiable content tree")
	}
}

func TestValidationAllSkippedIsNotPassing(t *testing.T) {
	if got := mapValidationState("skipped", "no Go packages or plugin paths were changed"); got != "SKIPPED" {
		t.Fatalf("all-skipped validation state=%q, want SKIPPED", got)
	}
}

func TestValidationFailReturnsFailingTestNames(t *testing.T) {
	got := runValidationFixture(t, "failed", errors.New("test failure"))
	if len(got.FailedTests) != 1 || got.FailedTests[0] != "TestBad" {
		t.Fatalf("failing tests=%v", got.FailedTests)
	}
}

func TestValidationLaunchError(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	prior := launchApplyValidation
	launches := 0
	launchApplyValidation = func([]string, []string, string) (int, int64, error) {
		launches++
		return 0, 0, errors.New("spawn denied")
	}
	t.Cleanup(func() { launchApplyValidation = prior })
	value := map[string]any{"applied": true, "paths": record.Paths}
	enqueueAppliedValidation(root, map[string]any{"jobId": record.JobID, "repoRoot": repo}, value)
	got, err := readApplyValidation(validationRecordPath(root, validationRepositoryKey(repo), record.JobID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "launch_error" || launches != 1 {
		t.Fatalf("record=%#v launches=%d", got, launches)
	}
}

func TestValidationDefaultHookRecordsWithoutLaunchingDetachedChild(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	launches := applyValidationTestLaunches.Load()
	value := map[string]any{"applied": true, "paths": record.Paths}
	enqueueAppliedValidation(root, map[string]any{"jobId": record.JobID, "repoRoot": repo}, value)
	if got := applyValidationTestLaunches.Load() - launches; got != 1 {
		t.Fatalf("default test hook calls=%d, want 1 recorded no-op", got)
	}
	if validation := value["validation"].(map[string]any); validation["status"] != "pending" {
		t.Fatalf("validation result=%#v", validation)
	}
}

func TestValidationRefusesGoTestExecutable(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	prior := applyValidationExecutable
	applyValidationExecutable = func() (string, error) { return filepath.Join(t.TempDir(), "jobs.test.exe"), nil }
	t.Cleanup(func() { applyValidationExecutable = prior })
	launches := applyValidationTestLaunches.Load()
	value := map[string]any{"applied": true, "paths": record.Paths}
	enqueueAppliedValidation(root, map[string]any{"jobId": record.JobID, "repoRoot": repo}, value)
	got, err := readApplyValidation(validationRecordPath(root, validationRepositoryKey(repo), record.JobID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "launch_error" || !strings.Contains(got.Reason, "test executable") || applyValidationTestLaunches.Load() != launches {
		t.Fatalf("record=%#v launches=%d", got, applyValidationTestLaunches.Load()-launches)
	}
}

func TestValidationChildEnvironmentMakesEnqueueNoop(t *testing.T) {
	t.Setenv(applyValidationChildEnv, "1")
	root, repo, record := validationFixture(t, []string{"main.go"})
	launches := applyValidationTestLaunches.Load()
	value := map[string]any{"applied": true, "paths": record.Paths}
	enqueueAppliedValidation(root, map[string]any{"jobId": record.JobID, "repoRoot": repo}, value)
	if applyValidationTestLaunches.Load() != launches {
		t.Fatalf("child environment attempted a detached launch")
	}
	if _, err := os.Stat(validationRecordPath(root, validationRepositoryKey(repo), record.JobID)); !os.IsNotExist(err) {
		t.Fatalf("child environment wrote a validation record: %v", err)
	}
}

func TestValidationEnqueueFailureReturnsResultWithoutDurableValidation(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "missing-repository")
	id := strings.Repeat("b", 64)
	jobDir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(jobDir, id+".out"), "done")
	jobBytes, err := json.Marshal(map[string]any{"jobId": id, "state": "SUCCEEDED", "repoRoot": repo,
		"outputFile": id + ".out", "threadId": "thread", "finishedAt": float64(time.Now().Unix())})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(jobDir, id+".json"), jobBytes, 0600); err != nil {
		t.Fatal(err)
	}
	value := map[string]any{"applied": true, "paths": []string{"main.go"}}
	enqueueAppliedValidation(root, map[string]any{"jobId": id, "repoRoot": repo}, value)
	validation, ok := value["validation"].(map[string]any)
	if !ok || validation["status"] != "launch_error" {
		t.Fatalf("validation result missing from apply result: %#v", value)
	}
	result := resultJob(jobDir, id, float64(time.Now().Unix()), ResultOptions{StateRoot: root})["structuredContent"].(map[string]any)
	if got, ok := result["validation"].(map[string]any); !ok || got["status"] != "launch_error" {
		t.Fatalf("repository-backed job result omitted validation: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "apply_validation")); !os.IsNotExist(err) {
		t.Fatalf("unexpected durable validation record: %v", err)
	}
}

func TestValidationTimeout(t *testing.T) {
	runValidationFixture(t, "timeout", context.DeadlineExceeded)
}

func TestValidationCommandHelper(t *testing.T) {
	switch os.Getenv("PURSER_VALIDATION_COMMAND_HELPER") {
	case "worker":
		_, _ = io.WriteString(os.Stdout, "worker output before exit\n")
		child := exec.Command(os.Args[0], "-test.run=^TestValidationCommandHelper$")
		child.Env = replaceValidationEnv(os.Environ(), "PURSER_VALIDATION_COMMAND_HELPER", "grandchild")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
	case "grandchild":
		time.Sleep(30 * time.Second)
	case "timeout":
		time.Sleep(30 * time.Second)
	}
}

func TestValidationCommandExitedWhileGrandchildKeepsPipeOpen(t *testing.T) {
	log, err := os.CreateTemp(t.TempDir(), "validation.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	env := replaceValidationEnv(os.Environ(), "PURSER_VALIDATION_COMMAND_HELPER", "worker")
	started := time.Now()
	err = runValidationCommand(t.TempDir(), env, []string{os.Args[0], "-test.run=^TestValidationCommandHelper$"}, 10*time.Second, log, nil)
	if err != nil {
		t.Fatalf("runValidationCommand: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("command completion took %s", elapsed)
	}
	if _, err := log.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "worker output before exit") {
		t.Fatalf("output before worker exit was not captured: %q", output)
	}
}

func TestValidationCommandDeadlineWhileProcessRunning(t *testing.T) {
	log, err := os.CreateTemp(t.TempDir(), "validation.log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	env := replaceValidationEnv(os.Environ(), "PURSER_VALIDATION_COMMAND_HELPER", "timeout")
	err = runValidationCommand(t.TempDir(), env, []string{os.Args[0], "-test.run=^TestValidationCommandHelper$"}, 100*time.Millisecond, log, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runValidationCommand error = %v, want deadline exceeded", err)
	}
}

func TestValidationStaleRevision(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	key := validationRepositoryKey(repo)
	path := validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "main.go"), "package validation\nfunc Value() int { return 3 }\n")
	prior := runApplyValidationCommand
	called := false
	runApplyValidationCommand = func(string, []string, []string, time.Duration, io.Writer, *applyValidation) error {
		called = true
		return nil
	}
	t.Cleanup(func() { runApplyValidationCommand = prior })
	if err := RunApplyValidation(root, key, record.JobID); err != nil {
		t.Fatal(err)
	}
	got, err := readApplyValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stale" || called {
		t.Fatalf("record=%#v command_called=%t", got, called)
	}
	root, repo, record = validationFixture(t, []string{"main.go"})
	key = validationRepositoryKey(repo)
	path = validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	prior = runApplyValidationCommand
	runApplyValidationCommand = func(_ string, _ []string, args []string, _ time.Duration, _ io.Writer, result *applyValidation) error {
		if len(args) > 0 && args[0] == "test" && result != nil {
			result.Counts = map[string]int{"tests_passed": 1}
		}
		return nil
	}
	t.Cleanup(func() { runApplyValidationCommand = prior })
	if err := RunApplyValidation(root, key, record.JobID); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "main.go"), "package validation\nfunc Value() int { return 4 }\n")
	if got := ApplyValidationResult(root, repo, record.JobID); got["status"] != "stale" {
		t.Fatalf("source edit retained validation receipt: %#v", got)
	}
}

func TestValidationSourceEditDuringRunIsStale(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	key := validationRepositoryKey(repo)
	path := validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	prior := runApplyValidationCommand
	runApplyValidationCommand = func(_ string, _ []string, args []string, _ time.Duration, _ io.Writer, result *applyValidation) error {
		if len(args) > 0 && args[0] == "test" {
			write(t, filepath.Join(repo, "main.go"), "package validation\nfunc Value() int { return 99 }\n")
			if result != nil {
				result.Counts = map[string]int{"tests_passed": 1}
			}
		}
		return nil
	}
	t.Cleanup(func() { runApplyValidationCommand = prior })
	if err := RunApplyValidation(root, key, record.JobID); err != nil {
		t.Fatal(err)
	}
	got, err := readApplyValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stale" {
		t.Fatalf("source edit during validation was not stale: %#v", got)
	}
}

func TestValidationCommitDuringRunKeepsPassedResult(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	key := validationRepositoryKey(repo)
	path := validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	prior := runApplyValidationCommand
	runApplyValidationCommand = func(_ string, _ []string, args []string, _ time.Duration, _ io.Writer, result *applyValidation) error {
		if len(args) > 0 && args[0] == "test" {
			git(t, repo, "add", "--all")
			git(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "applied changes")
			if result != nil {
				result.Counts = map[string]int{"tests_passed": 1}
			}
		}
		return nil
	}
	t.Cleanup(func() { runApplyValidationCommand = prior })
	if err := RunApplyValidation(root, key, record.JobID); err != nil {
		t.Fatal(err)
	}
	got, err := readApplyValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "passed" {
		t.Fatalf("commit of validated content changed status: %#v", got)
	}
}

func TestValidationRepeatApplyEnqueuesOnce(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	prior := launchApplyValidation
	launches := 0
	launchApplyValidation = func([]string, []string, string) (int, int64, error) { launches++; return 42, 1, nil }
	t.Cleanup(func() { launchApplyValidation = prior })
	job := map[string]any{"jobId": record.JobID, "repoRoot": repo}
	for i := 0; i < 2; i++ {
		value := map[string]any{"applied": true, "paths": record.Paths}
		enqueueAppliedValidation(root, job, value)
	}
	if launches != 1 {
		t.Fatalf("launches=%d", launches)
	}
}

func TestValidationInterruptedOnRestart(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	path := validationRecordPath(root, validationRepositoryKey(repo), record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	RecoverApplyValidations(root)
	got, err := readApplyValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || got.Reason != "validation process no longer matches its recorded identity" {
		t.Fatalf("record=%+v", got)
	}
}

func TestValidationWaitsForRepositoryValidationLock(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"main.go"})
	key := validationRepositoryKey(repo)
	path := validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	unlock, err := contextio.LockFileBlocking(filepath.Join(root, "apply_validation", key, "repository.lock"))
	if err != nil {
		t.Fatal(err)
	}
	prior := runApplyValidationCommand
	runApplyValidationCommand = func(_ string, _ []string, args []string, _ time.Duration, _ io.Writer, result *applyValidation) error {
		if len(args) > 0 && args[0] == "test" && result != nil {
			result.Counts = map[string]int{"tests_passed": 1}
		}
		return nil
	}
	t.Cleanup(func() {
		runApplyValidationCommand = prior
		_ = unlock()
	})
	finished := make(chan error, 1)
	go func() { finished <- RunApplyValidation(root, key, record.JobID) }()
	time.Sleep(11 * time.Second)
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("validation exited while queued behind another validation: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("validation did not finish after repository lock was released")
	}
	got, err := readApplyValidation(path)
	if err != nil || got.Status != "passed" {
		t.Fatalf("validation after lock release = %#v, %v", got, err)
	}
}

func TestValidationMissingPluginBinaryIsSkipped(t *testing.T) {
	root, repo, record := validationFixture(t, []string{"hooks/register.ts"})
	if err := os.MkdirAll(filepath.Join(repo, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, "hooks", "register.ts"), "export {}\n")
	revision, snapshot, err := checkoutRevision(repo)
	if err != nil {
		t.Fatal(err)
	}
	record.Revision, record.Snapshot = revision, snapshot
	key := validationRepositoryKey(repo)
	path := validationRecordPath(root, key, record.JobID)
	if err := writeApplyValidation(path, record); err != nil {
		t.Fatal(err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(gitBin))
	if err := RunApplyValidation(root, key, record.JobID); err != nil {
		t.Fatal(err)
	}
	got, err := readApplyValidation(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "skipped" || got.Reason != "claude binary is unavailable" {
		t.Fatalf("record=%#v", got)
	}
	if summary := validationSummary(got); summary["plugin_test"].(map[string]any)["status"] != "skipped" {
		t.Fatalf("plugin absence was not recorded separately: %#v", summary)
	}
}
