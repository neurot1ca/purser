package review

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
		Results     []CheckResult   `json:"results"`
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := RunChecks("../../..", fixture.Definitions, ctx)
	for i := range got {
		got[i].DurationMS = 0
	}
	if !equalJSON(got, fixture.Results) {
		t.Fatalf("results %#v want %#v", got, fixture.Results)
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
		err := exec.Command("powershell.exe", "-NoProfile", "-Command", "$p=Get-Process -Id "+pid+" -ErrorAction SilentlyContinue; if ($p) { exit 0 }; exit 1").Run()
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
