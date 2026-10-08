package review

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/profile"
)

func TestChecksParityFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/checks.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Definitions []profile.Check `json:"definitions"`
		Changed     []string        `json:"changed_paths"`
		Selected    []string        `json:"selected"`
		Cases       []struct {
			Name     string            `json:"name"`
			Tracked  map[string]string `json:"tracked_files"`
			PreFiles map[string]string `json:"pre_files"`
			NoGit    bool              `json:"no_git"`
			Argv     []string          `json:"argv"`
			Expected ChecksReport      `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	selected := SelectChecks(fixture.Definitions, fixture.Changed)
	names := make([]string, len(selected))
	for i, check := range selected {
		names[i] = check.Name
	}
	if !equalJSON(names, fixture.Selected) {
		t.Fatalf("selected %v want %v", names, fixture.Selected)
	}
	python := pythonPath(t)
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		repo, err := filepath.Abs(filepath.Join(working, "..", "..", "..", ".tmp", "checks-oracle", item.Name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(repo, 0700); err != nil {
			t.Fatal(err)
		}
		if !item.NoGit {
			if err := testutil.Hide(exec.Command("git", "-C", repo, "init", "-q")).Run(); err != nil {
				t.Fatal(err)
			}
			if testutil.Hide(exec.Command("git", "-C", repo, "rev-parse", "--verify", "HEAD")).Run() == nil {
				cmd := testutil.Hide(exec.Command("git", "-C", repo, "reset", "--hard"))
				if err := cmd.Run(); err != nil {
					t.Fatal(err)
				}
			}
			if err := testutil.Hide(exec.Command("git", "-C", repo, "clean", "-fdx")).Run(); err != nil {
				t.Fatal(err)
			}
			for name, content := range item.Tracked {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if len(item.Tracked) > 0 {
				for _, args := range [][]string{{"config", "user.email", "parity@example.invalid"}, {"config", "user.name", "Parity"}, {"add", "."}, {"commit", "--allow-empty", "-qm", "baseline"}} {
					cmd := testutil.Hide(exec.Command("git", append([]string{"-C", repo}, args...)...))
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, output)
					}
				}
			}
			for name, content := range item.PreFiles {
				if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		argv := append([]string(nil), item.Argv...)
		if len(argv) > 0 && argv[0] == "__PYTHON__" {
			argv[0] = python
		}
		oldCeiling, hadCeiling := os.LookupEnv("GIT_CEILING_DIRECTORIES")
		if item.NoGit {
			if err := os.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(repo)); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		got := RunChecksReport(repo, []profile.Check{{Name: item.Name, Argv: argv, TimeoutSeconds: 5}}, ctx)
		cancel()
		if item.NoGit {
			if hadCeiling {
				_ = os.Setenv("GIT_CEILING_DIRECTORIES", oldCeiling)
			} else {
				_ = os.Unsetenv("GIT_CEILING_DIRECTORIES")
			}
		}
		for i := range got.Results {
			got.Results[i].DurationMS = 0
		}
		if !equalJSON(got, item.Expected) {
			t.Fatalf("case %s report %#v want %#v", item.Name, got, item.Expected)
		}
	}
}

func TestRunChecksEmptyArgvIsUnknown(t *testing.T) {
	got := RunChecks(t.TempDir(), []profile.Check{{Name: "empty-argv", TimeoutSeconds: 1}}, context.Background())
	if len(got) != 1 || got[0].Status != "UNKNOWN" || got[0].Tail != "launch error: argv is empty" {
		t.Fatalf("empty argv result %#v", got)
	}
}

func TestRunChecksFailureAndTimeoutKillChild(t *testing.T) {
	python := pythonPath(t)
	failed := RunChecks(t.TempDir(), []profile.Check{{Name: "bad", Argv: []string{python, "-c", "print('bad'); raise SystemExit(7)"}, TimeoutSeconds: 2}}, context.Background())
	if failed[0].Status != "FAIL" || failed[0].ExitCode == nil || *failed[0].ExitCode != 7 || strings.TrimSpace(failed[0].Tail) != "bad" {
		t.Fatalf("failure result %#v", failed[0])
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("CHECK_CHILD_PID", pidFile)
	script := "import os,subprocess,sys,time; c=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL); open(os.environ['CHECK_CHILD_PID'],'w').write(str(c.pid)); time.sleep(60)"
	got := RunChecks(t.TempDir(), []profile.Check{{Name: "tree", Argv: []string{python, "-c", script}, TimeoutSeconds: 1}}, context.Background())
	if got[0].Status != "UNKNOWN" || !strings.HasSuffix(got[0].Tail, "\ntimed out") {
		t.Fatalf("timeout result %#v", got[0])
	}
	pid := waitCheckPID(t, pidFile)
	if checkProcessExists(pid) {
		t.Fatalf("child process %s remains", pid)
	}
}

func TestRunChecksSharedDeadlineAndCancellation(t *testing.T) {
	python := pythonPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got := RunChecks(t.TempDir(), []profile.Check{
		{Name: "first", Argv: []string{python, "-c", "import time; time.sleep(2)"}, TimeoutSeconds: 5},
		{Name: "second", Argv: []string{python, "-c", "pass"}, TimeoutSeconds: 5},
	}, ctx)
	if got[0].Status != "UNKNOWN" || !strings.HasSuffix(got[0].Tail, "\ntimed out") || got[1].Tail != "not run: deadline" {
		t.Fatalf("deadline results %#v", got)
	}
	pidFile := filepath.Join(t.TempDir(), "cancel.pid")
	t.Setenv("CHECK_CHILD_PID", pidFile)
	childScript := "import os,subprocess,sys,time; c=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL); open(os.environ['CHECK_CHILD_PID'],'w').write(str(c.pid)); time.sleep(60)"
	cancelCtx, stop := context.WithCancel(context.Background())
	done := make(chan []CheckResult, 1)
	go func() {
		done <- RunChecks(t.TempDir(), []profile.Check{{Name: "cancel", Argv: []string{python, "-c", childScript}, TimeoutSeconds: 10}}, cancelCtx)
	}()
	pid := waitCheckPID(t, pidFile)
	stop()
	result := <-done
	if result[0].Status != "UNKNOWN" || !strings.HasSuffix(result[0].Tail, "\ntimed out") || checkProcessExists(pid) {
		t.Fatalf("cancellation result %#v child exists=%v", result[0], checkProcessExists(pid))
	}
}

func pythonPath(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python", "python3"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Fatal("python required")
	return ""
}
func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func waitCheckPID(t *testing.T, path string) string {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if b, e := os.ReadFile(path); e == nil {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file missing: %s", path)
	return ""
}
func checkProcessExists(pid string) bool {
	if runtime.GOOS == "windows" {
		err := testutil.Hide(exec.Command("powershell.exe", "-NoProfile", "-Command", "$p=Get-Process -Id "+pid+" -ErrorAction SilentlyContinue; if ($p) { exit 0 }; exit 1")).Run()
		if err == nil {
			return true
		}
		e, ok := err.(*exec.ExitError)
		return !ok || e.ExitCode() != 1
	}
	n, e := strconv.Atoi(pid)
	if e != nil {
		return true
	}
	p, e := os.FindProcess(n)
	if e != nil {
		return false
	}
	return p.Signal(os.Signal(nil)) == nil
}
