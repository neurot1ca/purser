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
	case "write-sleep":
		if err := os.WriteFile(os.Getenv("PURSER_CHECK_WRITE_FILE"), []byte("changed"), 0600); err != nil {
			os.Exit(8)
		}
		for {
			time.Sleep(time.Minute)
		}
	case "spawn-child-sleep":
		child := testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestChecksCommandHelper$"))
		child.Env = append(os.Environ(), "PURSER_CHECK_HELPER=write-after-delay")
		if err := child.Start(); err != nil {
			os.Exit(8)
		}
		for {
			time.Sleep(time.Minute)
		}
	case "write-after-delay":
		time.Sleep(1500 * time.Millisecond)
		if err := os.WriteFile(os.Getenv("PURSER_CHECK_WRITE_FILE"), []byte("alive"), 0600); err != nil {
			os.Exit(8)
		}
		os.Exit(0)
	default:
		os.Exit(9)
	}
}

func TestChecksSelectionAndProtectedGlobs(t *testing.T) {
	alwaysAndScoped := []profile.Check{
		{Name: "always"},
		{Name: "scoped", When: []string{"app/**", "**/*.ts"}},
	}
	for _, tc := range []struct {
		changed []string
		want    []string
	}{
		{[]string{"other/file.py"}, []string{"always"}},
		{[]string{"./app/deep/file.py"}, []string{"always", "scoped"}},
	} {
		got := SelectChecks(alwaysAndScoped, tc.changed)
		var names []string
		for _, check := range got {
			names = append(names, check.Name)
		}
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Errorf("SelectChecks(%v) = %v, want %v", tc.changed, names, tc.want)
		}
	}

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

func TestRunChecksPassEmptyAndDeadlineSideEffects(t *testing.T) {
	root := t.TempDir()
	if err := testutil.Hide(exec.Command("git", "-C", root, "init", "-q")).Run(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_CHECK_HELPER", "write")
	path := filepath.Join(root, "generated.txt")
	t.Setenv("PURSER_CHECK_WRITE_FILE", path)
	passed := RunChecksReport(root, []profile.Check{{Name: "write", Argv: checkHelperArgv(), TimeoutSeconds: 2}}, context.Background())
	if passed.Status != "PASS" || passed.Results[0].Tail != "" || strings.Join(passed.SideEffectPaths, ",") != "generated.txt" {
		t.Fatalf("passing check report %#v", passed)
	}
	empty := RunChecksReport(root, nil, context.Background())
	if empty.Status != "NOT_RUN" || len(empty.Results) != 0 || len(empty.SideEffectPaths) != 0 {
		t.Fatalf("empty check report %#v", empty)
	}

	_ = os.Remove(path)
	t.Setenv("PURSER_CHECK_HELPER", "write-sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	exhausted := RunChecksReport(root, []profile.Check{{Name: "write-sleep", Argv: checkHelperArgv(), TimeoutSeconds: 5}}, ctx)
	if exhausted.Status != "UNKNOWN" || exhausted.Results[0].Status != "UNKNOWN" || strings.Join(exhausted.SideEffectPaths, ",") != "generated.txt" {
		t.Fatalf("deadline report %#v", exhausted)
	}
}

func TestRunChecksFindsContentChangesWithStableGitStatus(t *testing.T) {
	root := t.TempDir()
	if err := testutil.Hide(exec.Command("git", "-C", root, "init", "-q")).Run(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"}} {
		if err := testutil.Hide(exec.Command("git", append([]string{"-C", root}, args...)...)).Run(); err != nil {
			t.Fatal(err)
		}
	}
	tracked := filepath.Join(root, "edited.txt")
	untouched := filepath.Join(root, "untouched.txt")
	if err := os.WriteFile(tracked, []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untouched, []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "edited.txt", "untouched.txt"}, {"commit", "-qm", "baseline"}} {
		if err := testutil.Hide(exec.Command("git", append([]string{"-C", root}, args...)...)).Run(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tracked, []byte("already dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untouched, []byte("also dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, initial string }{
		{"tracked", "edited.txt", "already dirty\n"},
		{"untracked", "untracked.txt", "before check\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.path)
			if tc.name == "untracked" {
				if err := os.WriteFile(path, []byte(tc.initial), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PURSER_CHECK_HELPER", "write")
			t.Setenv("PURSER_CHECK_WRITE_FILE", path)
			got := RunChecksReport(root, []profile.Check{{Name: "write", Argv: checkHelperArgv(), TimeoutSeconds: 2}}, context.Background())
			if len(got.SideEffectPaths) != 1 || got.SideEffectPaths[0] != tc.path {
				t.Fatalf("stable-status content change report %#v", got)
			}
		})
	}
}

func TestRunChecksFingerprintCapFallsBackToStatusOnly(t *testing.T) {
	root := t.TempDir()
	if err := testutil.Hide(exec.Command("git", "-C", root, "init", "-q")).Run(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.email", "test@example.invalid"}, {"config", "user.name", "Test"}} {
		if err := testutil.Hide(exec.Command("git", append([]string{"-C", root}, args...)...)).Run(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 501; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d.txt", i)), []byte("baseline\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := testutil.Hide(exec.Command("git", "-C", root, "add", ".")).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := testutil.Hide(exec.Command("git", "-C", root, "commit", "-qm", "baseline")).CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	for i := 0; i < 501; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%03d.txt", i)), []byte("dirty\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PURSER_CHECK_HELPER", "write")
	t.Setenv("PURSER_CHECK_WRITE_FILE", filepath.Join(root, "file-000.txt"))
	got := RunChecksReport(root, []profile.Check{{Name: "write", Argv: checkHelperArgv(), TimeoutSeconds: 3}}, context.Background())
	if len(got.SideEffectPaths) != 0 {
		t.Fatalf("fingerprint cap should fall back to status-only report: %#v", got)
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

func TestRunChecksTimeoutKillsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant.txt")
	t.Setenv("PURSER_CHECK_WRITE_FILE", marker)
	got := runCheckHelper(t, "spawn-child-sleep", time.Second, "")
	if got.Status != "UNKNOWN" || !strings.HasSuffix(got.Tail, "\ntimed out") {
		t.Fatalf("timeout result %#v", got)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("timed out check descendant wrote marker: %v", err)
	}
}

func TestRunChecksSharedDeadlineAndCancellation(t *testing.T) {
	t.Setenv("PURSER_CHECK_HELPER", "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	got := RunChecks(t.TempDir(), []profile.Check{
		{Name: "first", Argv: checkHelperArgv(), TimeoutSeconds: 5},
		{Name: "second", Argv: checkHelperArgv(), TimeoutSeconds: 5},
	}, ctx)
	if got[0].Status != "UNKNOWN" || !strings.HasSuffix(got[0].Tail, "\ntimed out") || got[1].Tail != "not run: deadline" {
		t.Fatalf("deadline results %#v", got)
	}

	cancelCtx, stop := context.WithCancel(context.Background())
	cancelPIDFile := filepath.Join(t.TempDir(), "cancel.pid")
	t.Setenv("PURSER_CHECK_PID_FILE", cancelPIDFile)
	done := make(chan []CheckResult, 1)
	go func() {
		done <- RunChecks(t.TempDir(), []profile.Check{{Name: "cancel", Argv: checkHelperArgv(), TimeoutSeconds: 10}}, cancelCtx)
	}()
	waitCheckPID(t, cancelPIDFile)
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
		cmd := testutil.Hide(exec.Command("powershell.exe", "-NoProfile", "-Command", "$p=Get-Process -Id "+pid+" -ErrorAction SilentlyContinue; if ($p) { exit 0 }; exit 1"))
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
