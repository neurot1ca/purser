package contextio

import "purser-hook/internal/testutil"

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceNoticeDropsResolvedFindings(t *testing.T) {
	repo := t.TempDir()
	command := testutil.Hide(exec.Command("git", "-C", repo, "init", "-q"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	tracked := filepath.Join(repo, "artifact.db")
	if err := os.WriteFile(tracked, []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "artifact.db"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "baseline", "-q"}} {
		command = testutil.Hide(exec.Command("git", append([]string{"-C", repo}, args...)...))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	for _, args := range [][]string{{"rm", "--cached", "artifact.db", "-q"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "untrack artifact", "-q"}} {
		command = testutil.Hide(exec.Command("git", append([]string{"-C", repo}, args...)...))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	notice := map[string]any{"finding_count": 2, "findings": []any{
		map[string]any{"path_line": "artifact.db:1", "evidence": "Tracked database"},
		map[string]any{"path_line": "gone/run.py:1", "evidence": "untracked files"},
	}}
	if got := reverifyMaintenanceFindings(repo, notice); got != 0 {
		t.Fatalf("remaining findings=%d, want 0", got)
	}
	if notice["done"] != true || len(notice["findings"].([]any)) != 0 {
		t.Fatalf("resolved report was not marked done and emptied: %#v", notice)
	}
}

func TestMaintenanceNoticeKeepsPresentUntrackedDirectoryFinding(t *testing.T) {
	repo := t.TempDir()
	command := testutil.Hide(exec.Command("git", "-C", repo, "init", "-q"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	dir := filepath.Join(repo, "scratch")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "run.py")
	if err := os.WriteFile(path, []byte("print('ok')"), 0600); err != nil {
		t.Fatal(err)
	}
	notice := map[string]any{"finding_count": 1, "findings": []any{
		map[string]any{"path_line": "scratch/run.py:1", "evidence": "untracked files"},
	}}
	if got := reverifyMaintenanceFindings(repo, notice); got != 1 {
		t.Fatalf("remaining findings=%d, want 1", got)
	}
	if notice["done"] == true || notice["resolved"] == true {
		t.Fatalf("present untracked finding was marked resolved: %#v", notice)
	}
}

func TestMaintenanceNoticeKeepsFindingWhenGitReverificationFails(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "artifact.db")
	if err := os.WriteFile(path, []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: C:\\does-not-exist"), 0600); err != nil {
		t.Fatal(err)
	}
	notice := map[string]any{"report_path": filepath.Join(repo, "report.md"), "finding_count": 1, "findings": []any{
		map[string]any{"path_line": "artifact.db:1", "evidence": "tracked database"},
	}}
	if err := os.WriteFile(str(notice["report_path"]), []byte("- Source commit: `0123456789012345678901234567890123456789`\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := append([]any(nil), notice["findings"].([]any)...)
	if _, ok := dropStaleMaintenanceFindings(repo, notice); ok {
		t.Fatal("invalid repository unexpectedly verified a diff")
	}
	remaining, _ := notice["findings"].([]any)
	unchanged := len(remaining) == 1 && remaining[0].(map[string]any)["path_line"] == before[0].(map[string]any)["path_line"]
	if notice["finding_count"] != 1 || notice["done"] == true || notice["resolved"] == true || !unchanged {
		t.Fatalf("Git failure changed notice: %#v", notice)
	}
}

func TestMaintenanceNoticeDropsAllStaleFindings(t *testing.T) {
	repo := maintenanceNoticeGitRepo(t)
	maintenanceNoticeWrite(t, repo, "hooks/mod/state-root.ts", "source")
	maintenanceNoticeWrite(t, repo, "go/internal/release/smoke.go", "source")
	maintenanceNoticeCommit(t, repo, "baseline")
	source := maintenanceNoticeHead(t, repo)
	maintenanceNoticeWrite(t, repo, "hooks/mod/state-root.test.ts", "test")
	maintenanceNoticeWrite(t, repo, "go/internal/release/smoke_test.go", "test")
	maintenanceNoticeCommit(t, repo, "tests")
	notice := maintenanceNoticeFixture(t, repo, source, []string{"hooks/mod/state-root.ts:1", "go/internal/release/smoke.go:1"})
	if got, ok := dropStaleMaintenanceFindings(repo, notice); !ok || got != 0 {
		t.Fatalf("remaining=%d verified=%v, want 0,true", got, ok)
	}
	if len(notice["findings"].([]any)) != 0 || notice["finding_count"] != 0 || notice["done"] != true || notice["resolved"] != true {
		t.Fatalf("stale findings remain: %#v", notice)
	}
}

func TestMaintenanceNoticeFiltersSomeStaleFindings(t *testing.T) {
	repo := maintenanceNoticeGitRepo(t)
	maintenanceNoticeWrite(t, repo, "go/internal/contextio/lock.go", "source")
	maintenanceNoticeWrite(t, repo, "go/internal/contextio/maintenance_pid_windows.go", "source")
	maintenanceNoticeWrite(t, repo, "go/internal/contextio/untouched.go", "source")
	maintenanceNoticeCommit(t, repo, "baseline")
	source := maintenanceNoticeHead(t, repo)
	maintenanceNoticeWrite(t, repo, "go/internal/contextio/lock_windows_test.go", "test")
	maintenanceNoticeCommit(t, repo, "add test")
	notice := maintenanceNoticeFixture(t, repo, source, []string{
		"go/internal/contextio/lock.go:1",
		"go/internal/contextio/maintenance_pid_windows.go:1",
		"go/internal/contextio/untouched.go:1",
	})
	if got, ok := dropStaleMaintenanceFindings(repo, notice); !ok || got != 2 {
		t.Fatalf("remaining=%d verified=%v, want 2,true", got, ok)
	}
	findings := notice["findings"].([]any)
	if notice["finding_count"] != 2 || findings[0].(map[string]any)["path_line"] != "go/internal/contextio/maintenance_pid_windows.go:1" || findings[1].(map[string]any)["path_line"] != "go/internal/contextio/untouched.go:1" {
		t.Fatalf("remaining rows/count mismatch: %#v", notice)
	}
}

func maintenanceNoticeGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	command := testutil.Hide(exec.Command("git", "-C", repo, "init", "-q"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return repo
}

func maintenanceNoticeWrite(t *testing.T, repo, name, value string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func maintenanceNoticeCommit(t *testing.T, repo, message string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", message, "-q"}} {
		command := testutil.Hide(exec.Command("git", append([]string{"-C", repo}, args...)...))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
}

func maintenanceNoticeHead(t *testing.T, repo string) string {
	t.Helper()
	command := testutil.Hide(exec.Command("git", "-C", repo, "rev-parse", "HEAD"))
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func maintenanceNoticeFixture(t *testing.T, repo, source string, paths []string) map[string]any {
	t.Helper()
	findings := make([]any, 0, len(paths))
	for _, path := range paths {
		findings = append(findings, map[string]any{"path_line": path, "evidence": "missing test"})
	}
	reportPath := filepath.Join(repo, "report.md")
	if err := os.WriteFile(reportPath, []byte("- Source commit: `"+source+"`\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"report_path": reportPath, "finding_count": len(findings), "findings": findings}
}

func TestMaintenanceNoticeLegacyFindingHasReason(t *testing.T) {
	got := maintenanceWhy(map[string]any{"symbol": ".coverage", "evidence": "Tracked binary coverage artifact"})
	if got == "—" || got == "" {
		t.Fatalf("missing deterministic reason: %q", got)
	}
}

func TestMaintenanceNoticeEvidenceTruncatesAtWordBoundary(t *testing.T) {
	got := truncateMaintenanceEvidence("Tracked 1,568,768-byte SQLite coverage database; added as binary in bdafda8", 48)
	if strings.Contains(got, "databas") || strings.HasSuffix(got, " ") || !strings.HasSuffix(got, "…") {
		t.Fatalf("evidence was cut mid-word or lacks ellipsis: %q", got)
	}
}

func TestMaintenanceShowCapsTotalShowsAcrossQuotaCycles(t *testing.T) {
	notice := map[string]any{"cycles": map[string]any{
		"reset-1": map[string]any{"shows": 1}, "reset-2": map[string]any{"shows": 1}, "reset-3": map[string]any{"shows": 1},
	}}
	window := &[3]any{"5h", float64(90), "reset-4"}
	if show, _, _ := maintenanceShow(notice, map[string]any{}, window); show {
		t.Fatal("report was shown again after its report-wide show limit")
	}
}

func TestMaintenanceNoticeCoverageTargetAndFindingOrder(t *testing.T) {
	for _, test := range []struct {
		name      string
		coverage  any
		target    any
		celebrate bool
	}{{"override boundary", float64(80), float64(80), true}, {"below override", float64(79.9), float64(80), false}, {"null coverage", nil, float64(80), false}, {"legacy boundary", float64(90), nil, true}} {
		t.Run(test.name, func(t *testing.T) {
			notice := map[string]any{"report_type": "test-gaps", "finding_count": 0, "findings": []any{}}
			if test.coverage != nil {
				notice["line_coverage_pct"] = test.coverage
			}
			if test.target != nil {
				notice["coverage_target_pct"] = test.target
			}
			got := strings.Contains(formatMaintenanceNotice(notice), "code is now")
			if got != test.celebrate {
				t.Fatalf("celebration=%v, want %v", got, test.celebrate)
			}
		})
	}
	findings := []any{
		map[string]any{"path_line": "a.py:1", "symbol": "first", "evidence": "e", "confidence": "low", "confidence_rank": 1},
		map[string]any{"path_line": "b.py:2", "symbol": "second", "evidence": "e", "confidence": "high", "confidence_rank": 3},
	}
	for _, test := range []struct {
		reportType string
		first      string
		second     string
	}{{"test-gaps", "b.py:2 second", "a.py:1 first"}, {"cleanup", "b.py:2 second", "a.py:1 first"}} {
		text := formatMaintenanceNotice(map[string]any{"report_type": test.reportType, "finding_count": 2, "findings": findings})
		first, second := strings.Index(text, test.first), strings.Index(text, test.second)
		if first < 0 || second < 0 || first > second {
			t.Fatalf("%s notice order mismatch for %q before %q: %s", test.reportType, test.first, test.second, text)
		}
	}
}

func TestMaintenanceNoticeWhyColumnsAndLegacyFindings(t *testing.T) {
	master := `> *Say "turn off maintenance" to stop these reports, or run one any time with /purser:optimize.*`
	for _, test := range []struct {
		reportType string
		header     string
	}{
		{"cleanup", "> | Severity | Finding | Why clean it | Evidence |"},
		{"test-gaps", "> | Severity | Gap | Why it matters | Evidence |"},
	} {
		t.Run(test.reportType+" legacy", func(t *testing.T) {
			text := formatMaintenanceNotice(map[string]any{
				"report_type":   test.reportType,
				"finding_count": 1,
				"findings":      []any{map[string]any{"path_line": "a.py:1", "symbol": "a", "evidence": "e", "confidence": "high"}},
			})
			if !strings.Contains(text, test.header) || !strings.Contains(text, "> | high | a.py:1 a | reported as unused by the cleanup scan | e |") || strings.Contains(text, "| a.py:1 a | — | e |") || !strings.Contains(text, master) {
				t.Fatalf("legacy notice missing reason column or master instruction: %s", text)
			}
		})
		t.Run(test.reportType+" why truncation", func(t *testing.T) {
			why := strings.Repeat("界", 70)
			evidence := strings.Repeat("e", 60)
			text := formatMaintenanceNotice(map[string]any{
				"report_type":   test.reportType,
				"finding_count": 1,
				"findings":      []any{map[string]any{"path_line": "a.py:1", "symbol": "a", "why": why, "evidence": evidence, "confidence": "high"}},
			})
			row := "| high | a.py:1 a | " + strings.Repeat("界", 64) + " | " + strings.Repeat("e", 47) + "… |"
			if !strings.Contains(text, row) {
				t.Fatalf("notice did not apply rune limits: %s", text)
			}
		})
	}
}
