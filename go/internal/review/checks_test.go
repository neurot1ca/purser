package review

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/profile"
	"purser-hook/internal/testutil"
)

func TestChecksCommandHelper(t *testing.T) {
	mode := os.Getenv("PURSER_CHECK_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "go check failed")
		os.Exit(7)
	case "sleep":
		if path := os.Getenv("PURSER_CHECK_PID_FILE"); path != "" {
			if err := os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
				os.Exit(8)
			}
		}
		for {
			time.Sleep(time.Minute)
		}
	case "write":
		if err := os.WriteFile(os.Getenv("PURSER_CHECK_WRITE_FILE"), []byte("changed"), 0600); err != nil {
			os.Exit(8)
		}
		os.Exit(0)
	default:
		os.Exit(9)
	}
}

func TestChecksSelectionAndProtectedGlobs(t *testing.T) {
	checks := []profile.Check{
		{Name: "go", When: []string{"go/**"}},
		{Name: "docs", When: []string{"docs/**"}},
		{Name: "protected", When: []string{".claude/**"}},
	}
	for _, tc := range []struct {
		changed []string
		want    []string
	}{
		{[]string{"go/internal/review/checks.go"}, []string{"go"}},
		{[]string{"docs/readme.md", "go/main.go"}, []string{"go", "docs"}},
		{[]string{".claude/settings.json"}, []string{"protected"}},
		{[]string{"other/file.go"}, nil},
	} {
		got := SelectChecks(checks, tc.changed)
		var names []string
		for _, check := range got {
			names = append(names, check.Name)
		}
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Errorf("SelectChecks(%v) = %v, want %v", tc.changed, names, tc.want)
		}
	}
}

func TestRunChecksEmptyArgvIsUnknown(t *testing.T) {
	got := RunChecks(t.TempDir(), []profile.Check{{Name: "empty-argv", TimeoutSeconds: 1}}, context.Background())
	if len(got) != 1 || got[0].Status != "UNKNOWN" || got[0].Tail != "launch error: argv is empty" {
		t.Fatalf("empty argv result %#v", got)
	}
}

func TestRunChecksUnresolvableExecutableIsUnknown(t *testing.T) {
	got := RunChecks(t.TempDir(), []profile.Check{{Name: "missing", Argv: []string{"purser-missing-check-executable"}, TimeoutSeconds: 1}}, context.Background())
	if len(got) != 1 || got[0].Status != "UNKNOWN" || !strings.HasPrefix(got[0].Tail, "unresolvable executable:") {
		t.Fatalf("missing executable result %#v", got)
	}
}

func TestRunChecksReportsUntrackedAndTrackedChanges(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(fmt.Sprintf("tracked=%v", tracked), func(t *testing.T) {
			root := t.TempDir()
			if err := testutil.Hide(exec.Command("git", "-C", root, "init", "-q")).Run(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "changed.txt")
			if tracked {
				if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"}, {"add", "changed.txt"}, {"commit", "-qm", "baseline"}} {
					if out, err := testutil.Hide(exec.Command("git", append([]string{"-C", root}, args...)...)).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
				}
			}
			t.Setenv("PURSER_CHECK_HELPER", "write")
			t.Setenv("PURSER_CHECK_WRITE_FILE", path)
			got := RunChecksReport(root, []profile.Check{{Name: "write", Argv: checkHelperArgv(), TimeoutSeconds: 2}}, context.Background())
			if got.Status != "PASS" || len(got.SideEffectPaths) != 1 || got.SideEffectPaths[0] != "changed.txt" {
				t.Fatalf("report %#v", got)
			}
		})
	}
}

func TestRunChecksFailureAndTimeoutKillChild(t *testing.T) {
	failed := runCheckHelper(t, "fail", 2*time.Second, "")
	if failed.Status != "FAIL" || failed.ExitCode == nil || *failed.ExitCode != 7 || strings.TrimSpace(failed.Tail) != "go check failed" {
		t.Fatalf("failure result %#v", failed)
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	got := runCheckHelper(t, "sleep", time.Second, pidFile)
	if got.Status != "UNKNOWN" || !strings.HasSuffix(got.Tail, "\ntimed out") {
		t.Fatalf("timeout result %#v", got)
	}
	pid := waitCheckPID(t, pidFile)
	if processExists(pid) {
		t.Fatalf("timed out Go check process %s remains", pid)
	}
}

func TestRunChecksSharedDeadlineAndCancellation(t *testing.T) {
	t.Setenv("PURSER_CHECK_HELPER", "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got := RunChecks(t.TempDir(), []profile.Check{
		{Name: "first", Argv: checkHelperArgv(), TimeoutSeconds: 5},
		{Name: "second", Argv: checkHelperArgv(), TimeoutSeconds: 5},
	}, ctx)
	if got[0].Status != "UNKNOWN" || !strings.HasSuffix(got[0].Tail, "\ntimed out") || got[1].Tail != "not run: deadline" {
		t.Fatalf("deadline results %#v", got)
	}

	cancelCtx, stop := context.WithCancel(context.Background())
	done := make(chan []CheckResult, 1)
	go func() {
		done <- RunChecks(t.TempDir(), []profile.Check{{Name: "cancel", Argv: checkHelperArgv(), TimeoutSeconds: 10}}, cancelCtx)
	}()
	time.Sleep(100 * time.Millisecond)
	stop()
	result := <-done
	if result[0].Status != "UNKNOWN" || !strings.HasSuffix(result[0].Tail, "\ntimed out") {
		t.Fatalf("cancellation result %#v", result[0])
	}
}

func runCheckHelper(t *testing.T, mode string, timeout time.Duration, pidFile string) CheckResult {
	t.Helper()
	t.Setenv("PURSER_CHECK_HELPER", mode)
	if pidFile != "" {
		t.Setenv("PURSER_CHECK_PID_FILE", pidFile)
	}
	got := RunChecks(t.TempDir(), []profile.Check{{Name: mode, Argv: checkHelperArgv(), TimeoutSeconds: int(timeout.Seconds())}}, context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d results", len(got))
	}
	return got[0]
}

func checkHelperArgv() []string {
	return []string{os.Args[0], "-test.run=^TestChecksCommandHelper$"}
}

func waitCheckPID(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file missing: %s", path)
	return ""
}

func processExists(pid string) bool {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", "$p=Get-Process -Id "+pid+" -ErrorAction SilentlyContinue; if ($p) { exit 0 }; exit 1")
		err := cmd.Run()
		if err == nil {
			return true
		}
		exit, ok := err.(*exec.ExitError)
		return !ok || exit.ExitCode() != 1
	}
	n, err := strconv.Atoi(pid)
	if err != nil {
		return true
	}
	proc, err := os.FindProcess(n)
	if err != nil {
		return false
	}
	return proc.Signal(os.Signal(nil)) == nil
}
