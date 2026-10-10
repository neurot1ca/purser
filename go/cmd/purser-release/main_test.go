package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func releaseFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	commands := [][]string{{"init", "-q"}, {"config", "user.name", "Release Test"}, {"config", "user.email", "release@example.test"}, {"config", "core.autocrlf", "false"}}
	for _, args := range commands {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %v: %s", args, e, b)
		}
	}
	write := func(name, data string) {
		t.Helper()
		p := filepath.Join(root, name)
		if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte(data), 0644); e != nil {
			t.Fatal(e)
		}
	}
	write(".claude-plugin/plugin.json", "{\n  \"name\": \"purser\",\n  \"version\": \"0.1.12\"\n}\n")
	write("CHANGELOG.md", "# Changelog\n\n## 0.1.12 — 2026-09-24\n\n- Existing (`aaaaaaa`).\n")
	write("change.txt", "before\n")
	write("other.txt", "other\n")
	cmd := exec.Command("git", "-C", root, "add", ".")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("git add: %v: %s", e, b)
	}
	cmd = exec.Command("git", "-C", root, "-c", "user.name=Release Test", "-c", "user.email=release@example.test", "commit", "-qm", "base")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("git commit: %v: %s", e, b)
	}
	return root
}
func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v: %s", args, e, b)
	}
	return strings.TrimSpace(string(b))
}

func TestReleaseDryRunWithPathsDoesNotCommitOrChangeVersion(t *testing.T) {
	root := releaseFixture(t)
	if e := os.WriteFile(filepath.Join(root, "change.txt"), []byte("after\n"), 0644); e != nil {
		t.Fatal(e)
	}
	before := gitOutput(t, root, "rev-parse", "HEAD")
	if _, e := releaseVersion(root, "0.1.13", "Added change", "add change", true, filepath.Join(t.TempDir(), "market"), []string{"change.txt"}, "feature: add change"); e != nil {
		t.Fatal(e)
	}
	if got := gitOutput(t, root, "rev-parse", "HEAD"); got != before {
		t.Fatalf("dry run moved HEAD: %s -> %s", before, got)
	}
	b, e := os.ReadFile(filepath.Join(root, ".claude-plugin/plugin.json"))
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), `"version": "0.1.12"`) {
		t.Fatalf("dry run changed plugin version: %s", b)
	}
}

func TestReleasePathsCommitOnlyListedChangeAndResumeAfterReleaseCommit(t *testing.T) {
	root := releaseFixture(t)
	if e := os.WriteFile(filepath.Join(root, "change.txt"), []byte("selected\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "other.txt"), []byte("unselected edit\n"), 0644); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(t.TempDir(), "market")
	commit, e := releaseVersion(root, "0.1.13", "Added change", "add change", false, target, []string{"change.txt"}, "feature: add change")
	if e != nil {
		t.Fatal(e)
	}
	if got := gitOutput(t, root, "log", "-1", "--format=%s"); got != "purser: release 0.1.13 (add change)" {
		t.Fatalf("unexpected release commit: %s", got)
	}
	check := exec.Command("git", "-C", root, "diff", "--quiet", "--", "other.txt")
	if err := check.Run(); err == nil {
		t.Fatal("unlisted edit was committed")
	}
	changelog, e := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if e != nil {
		t.Fatal(e)
	}
	changeCommit := gitOutput(t, root, "rev-parse", "HEAD^")
	if !strings.Contains(string(changelog), "Added change (`"+changeCommit[:7]+"`).") {
		t.Fatalf("changelog omitted change commit hash: %s", changelog)
	}
	again, e := releaseVersion(root, "", "Added change", "add change", false, target, nil, "")
	if e != nil || again != commit {
		t.Fatalf("release retry did not resume HEAD: %s %v", again, e)
	}
}
