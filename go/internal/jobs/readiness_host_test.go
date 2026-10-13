package jobs

import "purser-hook/internal/testutil"

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

func TestProjectRoleReadinessUsesProjectExecutable(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	repository := filepath.Join(work, "repository")
	if err := os.MkdirAll(repository, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(work, "state")
	executable := filepath.ToSlash(filepath.Join(work, "project-reviewer"))
	roles := profile.ProjectRolesPath(state, repository)
	if err := os.MkdirAll(filepath.Dir(roles), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(roles, []byte("schema_version = 1\n[coding]\nreviewer1_executable = \""+executable+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", root)
	t.Setenv("USERPROFILE", filepath.Join(work, "home"))
	t.Setenv("PURSER_STATE_DIR", state)
	config, err := readinessCoderConfig(repository)
	if err != nil {
		t.Fatal(err)
	}
	if got := config["reviewer1_executable"]; got != executable {
		t.Fatalf("readiness executable = %#v, want %q", got, executable)
	}
}

func init() {
	if os.Getenv("PURser_READINESS_MOCK") == "1" {
		if marker := os.Getenv("PURser_READINESS_MARKER"); marker != "" {
			_ = os.WriteFile(marker, []byte("called"), 0600)
		}
		if os.Getenv("PURser_READINESS_SLEEP") == "1" {
			time.Sleep(2 * time.Second)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"conversation_id": "readiness-mock", "status": "success", "response": "{}",
			"duration_seconds": 0.1, "num_turns": 1, "usage": map[string]any{},
			"structured_output": map[string]any{"verdict": "NO_UNMET_FOUND", "criterion": nil, "evidence": []any{}},
		})
		os.Exit(0)
	}
}

func TestReadinessHostStoresGroundedPass(t *testing.T) {
	fixture := newReadinessHostFixture(t, "gpt-5", "agy", true)
	t.Setenv("PATH", fixture.gitDir)
	checkReadinessBeforeSuccess(fixture.jobs, fixture.id, map[string]any{"content": "worker report"})
	if _, err := os.Stat(fixture.marker); err != nil {
		t.Fatalf("absolute checker was not called: %v", err)
	}
	job := readReadinessJob(t, fixture.jobs, fixture.id)
	readiness := job["readinessCheck"].(map[string]any)
	result := readiness["result"].(map[string]any)
	if result["verdict"] != "NO_UNMET_FOUND" || readiness["checkedAt"] == nil {
		t.Fatalf("readiness result was not stored: %#v", readiness)
	}
	if len(readinessTelemetry(t, fixture.root)) != 1 {
		t.Fatal("READINESS_CHECK_OUTCOME telemetry was not emitted")
	}
}

func TestReadinessHostUngroundedDoesNotRun(t *testing.T) {
	fixture := newReadinessHostFixture(t, "gpt-5", "agy", false)
	t.Setenv("PURser_READINESS_MOCK", "")
	checkReadinessBeforeSuccess(fixture.jobs, fixture.id, nil)
	assertReadinessCheckerNotCalled(t, fixture.marker)
	job := readReadinessJob(t, fixture.jobs, fixture.id)
	if _, ok := job["readinessCheck"].(map[string]any)["result"]; ok {
		t.Fatal("ungrounded readiness was persisted")
	}
	if len(readinessTelemetry(t, fixture.root)) != 0 {
		t.Fatal("ungrounded readiness emitted telemetry")
	}
}

func TestReadinessHostAgentOffIsUnavailable(t *testing.T) {
	fixture := newReadinessHostFixture(t, "gpt-5", "off", true)
	checkReadinessBeforeSuccess(fixture.jobs, fixture.id, nil)
	assertReadinessCheckerNotCalled(t, fixture.marker)
	if got := readinessResult(t, fixture); got["verdict"] != "UNAVAILABLE" {
		t.Fatalf("agent off verdict = %#v", got)
	}
}

func TestReadinessHostSameFamilyRuns(t *testing.T) {
	fixture := newReadinessHostFixture(t, "gemini-2.5-pro", "agy", true)
	t.Setenv("PATH", fixture.gitDir)
	checkReadinessBeforeSuccess(fixture.jobs, fixture.id, nil)
	if _, err := os.Stat(fixture.marker); err != nil {
		t.Fatalf("same-family readiness checker was not called: %v", err)
	}
	if got := readinessResult(t, fixture); got["verdict"] != "NO_UNMET_FOUND" {
		t.Fatalf("same-family verdict = %#v", got)
	}
}

func TestReadinessHostMissingCheckerIsUnknown(t *testing.T) {
	fixture := newReadinessHostFixture(t, "gpt-5", "agy", true)
	t.Setenv("PATH", fixture.gitDir)
	missing := filepath.Join(t.TempDir(), "agy")
	if runtime.GOOS == "windows" {
		missing += ".exe"
	}
	if err := os.WriteFile(fixture.userConfig, []byte("[coding]\nreviewer1_executable = '"+missing+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURser_READINESS_MOCK", "")
	checkReadinessBeforeSuccess(fixture.jobs, fixture.id, nil)
	assertReadinessCheckerNotCalled(t, fixture.marker)
	if got := readinessResult(t, fixture); got["verdict"] != "UNKNOWN" {
		t.Fatalf("missing checker verdict = %#v, want UNKNOWN", got)
	}
}

func TestReadinessCheckerTimeoutIsUnknown(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("PURser_READINESS_MOCK", "1")
	t.Setenv("PURser_READINESS_SLEEP", "1")
	marker := filepath.Join(t.TempDir(), "checker-called")
	t.Setenv("PURser_READINESS_MARKER", marker)
	checker := readinessCheckerExecutable(t)
	got, err := review.RunReadiness(t.Context(), "", review.ReadinessRequest{
		Grounded: true, Objective: "test", AcceptanceCriteria: "must work", AttributedDiff: "+++ b/a\n@@ -0,0 +1 @@\n+x\n",
		Agent: "agy", Model: "gemini-2.5-pro", RepoRoot: t.TempDir(), Executable: checker,
	}, 300*time.Millisecond)
	if err == nil || got.Verdict != "UNKNOWN" {
		t.Fatalf("timeout got verdict=%s err=%v, want UNKNOWN with an error", got.Verdict, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("absolute checker was not called with PATH empty (run error %v): %v", err, err)
	}
}

func TestReadinessHostDoesNotWriteAfterJobStopsRunning(t *testing.T) {
	fixture := newReadinessHostFixture(t, "gpt-5", "agy", true)
	job := readReadinessJob(t, fixture.jobs, fixture.id)
	job["state"] = "SUCCEEDED"
	data, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(fixture.jobs, fixture.id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	checkReadinessBeforeSuccess(fixture.jobs, fixture.id, nil)
	job = readReadinessJob(t, fixture.jobs, fixture.id)
	if _, ok := job["readinessCheck"].(map[string]any)["result"]; ok {
		t.Fatal("terminal job readiness was written")
	}
}

type readinessHostFixture struct {
	root, jobs, id, userConfig, gitDir, marker string
}

func newReadinessHostFixture(t *testing.T, implementer, agent string, grounded bool) readinessHostFixture {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "plugin")
	configDir := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	userConfig := filepath.Join(home, ".orchestrator", "config.toml")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0700); err != nil {
		t.Fatal(err)
	}
	checker := readinessCheckerExecutable(t)
	if err := os.WriteFile(userConfig, []byte("[coding]\nreviewer1_executable = '"+checker+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	readinessGit(t, git, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	readinessGit(t, git, repo, "add", "--all")
	readinessGit(t, git, repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "baseline")
	snapshot := strings.TrimSpace(readinessGit(t, git, repo, "rev-parse", "HEAD^{tree}"))
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("baseline\nadded readiness line\n"), 0600); err != nil {
		t.Fatal(err)
	}

	jobs := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobs, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	job := map[string]any{"jobId": id, "state": "RUNNING", "taskId": "readiness-test", "model": implementer,
		"repoRoot": repo, "worktreePath": repo, "snapshot": snapshot,
		"readinessCheck": map[string]any{"grounded": grounded, "objective": "test", "acceptance_criteria": "must pass", "agent": agent, "model": "gemini-2.5-pro"}}
	data, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(jobs, id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", "")
	t.Setenv("PURser_READINESS_MOCK", "1")
	t.Setenv("PURser_READINESS_SLEEP", "")
	marker := filepath.Join(root, "checker-called")
	t.Setenv("PURser_READINESS_MARKER", marker)
	return readinessHostFixture{root: root, jobs: jobs, id: id, userConfig: userConfig, gitDir: filepath.Dir(git), marker: marker}
}

func readinessCheckerExecutable(t *testing.T) string {
	t.Helper()
	current, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	name := "agy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	checker := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(checker, data, 0700); err != nil {
		t.Fatal(err)
	}
	return checker
}

func assertReadinessCheckerNotCalled(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("checker call marker exists or cannot be inspected: %v", err)
	}
}

func readinessGit(t *testing.T, executable, root string, args ...string) string {
	t.Helper()
	command := testutil.Hide(exec.Command(executable, args...))
	command.Dir = root
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func readReadinessJob(t *testing.T, dir, id string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func readinessResult(t *testing.T, fixture readinessHostFixture) map[string]any {
	t.Helper()
	readiness := readReadinessJob(t, fixture.jobs, fixture.id)["readinessCheck"].(map[string]any)
	result, ok := readiness["result"].(map[string]any)
	if !ok {
		t.Fatalf("readiness result missing: %#v", readiness)
	}
	return result
}

func readinessTelemetry(t *testing.T, root string) []map[string]any {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root, "telemetry", "events", "*", "session_*.jsonl"))
	var events []map[string]any
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(line), &event) == nil && event["event_type"] == "READINESS_CHECK_OUTCOME" {
				events = append(events, event)
			}
		}
	}
	return events
}
