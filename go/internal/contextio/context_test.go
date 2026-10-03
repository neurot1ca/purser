package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestJournalWriteRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "turn.json")
	turn := newTurn("")
	if err := writeTurn(p, turn); err != nil {
		t.Fatal(err)
	}
	got, reason := loadTurn(p)
	if reason != "" || got == nil || num(got["schema_version"]) != 1 {
		t.Fatalf("loadTurn = (%v, %q)", got, reason)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"turn_nonce", "opened_at", "observations", "read_count", "full_read_count", "targeted_read_count", "unknown_read_count", "stat_operations", "bytes_scanned", "lines_scanned", "task_association_known", "task_id", "project_key", "codex_boundary_index", "unknown_reason", "latency"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing journal field %q", key)
		}
	}
}

func TestContextTurnEventRecordsGoRuntime(t *testing.T) {
	event := contextTurnEvent(newTurn(""), 0, 0, time.Now())
	payload, ok := event["payload"].(map[string]any)
	if !ok || payload["runtime"] != "go" {
		t.Fatalf("context turn runtime = %v, want go", payload["runtime"])
	}
}

func TestStopEventAppendRecoversOwnPartialRecord(t *testing.T) {
	root := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "go-stop-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "events.jsonl")
	event := map[string]any{"event_id": "one", "payload": map[string]any{"read_count": 1}}
	data, _ := json.Marshal(event)
	if err := os.WriteFile(path, data[:len(data)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if !appendEventOnce(path, event) || !appendEventOnce(path, event) {
		t.Fatal("append failed")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data)+"\n" {
		t.Fatalf("unexpected log: %q", got)
	}
}

func TestReadEdgeCases(t *testing.T) {
	turn := newTurn("")
	setReason(turn, "")
	if turn["unknown_reason"] != nil {
		t.Fatalf("empty reason changed null: %#v", turn["unknown_reason"])
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := expand("~"); got != filepath.Clean(home) {
		t.Fatalf("expand(~) = %q, want %q", got, home)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	usernameHome := home
	if runtime.GOOS == "windows" {
		usernameHome = filepath.Join(filepath.Dir(home), current.Username)
	}
	if got, want := expand("~"+current.Username+string(filepath.Separator)+"state"), filepath.Join(usernameHome, "state"); got != want {
		t.Fatalf("expand(~username) = %q, want %q", got, want)
	}
	p := filepath.Join(t.TempDir(), "oversized.json")
	turn["padding"] = strings.Repeat("x", maxJournal)
	err = writeTurn(p, turn)
	var sizeErr journalLimitError
	if !errors.As(err, &sizeErr) {
		t.Fatalf("oversized write error = %T %v", err, err)
	}
	binary := filepath.Join(t.TempDir(), "binary.txt")
	if err := os.WriteFile(binary, []byte("one\x00two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	obs := scanRead(newTurn(""), binary, info, "FULL", map[string]any{})
	if obs["line_kind"] != "UNKNOWN" || obs["binary_suspected"] != true || obs["reason"] != "BINARY_SUSPECTED" {
		t.Fatalf("binary observation = %#v", obs)
	}
}

func TestMaintenanceConsentUsesPluginRoot(t *testing.T) {
	pluginRoot := filepath.Join(t.TempDir(), "installed-plugin")
	projectRoot := filepath.Join(t.TempDir(), "user-project")
	if pluginRoot == projectRoot {
		t.Fatal("plugin and project roots must differ")
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("python is not on PATH")
	}
	python, _ = filepath.Abs(python)
	pluginScript := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "maintenance_reports.py")
	projectScript := filepath.Join(projectRoot, ".claude", "hooks", "orchestrator", "maintenance_reports.py")
	state := map[string]any{"maintenance_pending": map[string]any{
		"notice_file": filepath.Join(projectRoot, "maintenance", "consent.json"),
		"text":        "> ⚓ **Run it?**",
	}}
	got := lifecycleGuidance(state)
	if !strings.Contains(got, `"`+python+`" "`+pluginScript+`"`) {
		t.Fatalf("maintenance CLI did not use plugin script path %q: %s", pluginScript, got)
	}
	if strings.Contains(got, projectScript) {
		t.Fatalf("maintenance CLI used project script path %q: %s", projectScript, got)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(filepath.Dir(filepath.Dir(executable)), ".claude", "hooks", "orchestrator", "maintenance_reports.py")
	if got := maintenanceScriptPath(); got != fallback {
		t.Fatalf("maintenance script fallback = %q, want %q", got, fallback)
	}
}

func TestProjectRootWalk(t *testing.T) {
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE"} {
		value, set := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if set {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	nested := filepath.Join(repo, "one", "two")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if got := projectRoot(map[string]any{"cwd": nested}); got != resolve(repo) {
		t.Fatalf("nested root = %q, want %q", got, resolve(repo))
	}
	if out, err := exec.Command("git", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "seed").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	worktree := filepath.Join(base, "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--quiet", "--detach", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	if info, err := os.Stat(filepath.Join(worktree, ".git")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("worktree .git is not a file: %v", err)
	}
	if got := projectRoot(map[string]any{"cwd": worktree}); got != resolve(worktree) {
		t.Fatalf("worktree root = %q, want %q", got, resolve(worktree))
	}
	noRepo := filepath.VolumeName(base) + string(filepath.Separator)
	if got := projectRoot(map[string]any{"cwd": noRepo}); got != "" {
		t.Fatalf("no-repo cwd = %q, root = %q, want empty", noRepo, got)
	}
	t.Setenv("GIT_DIR", filepath.Join(base, "missing-git-dir"))
	if got := projectRoot(map[string]any{"cwd": nested}); got != "" {
		t.Fatalf("GIT_DIR override bypassed git: root = %q", got)
	}
}

func TestProtectedProfileParseFailure(t *testing.T) {
	for _, input := range []string{
		"schema_version = 1\n[paths]\nprotected = [\"private/**\"\n",
		"schema_version = 1\n[paths]\nprotected = 7\n",
		"schema_version = 2\n[paths]\nprotected = [\"private/**\"]\n",
		"schema_version = 1\n[paths]\nprotected = [\"bad\\x2e\"]\n",
	} {
		if paths, ok := parseProtectedProfile([]byte(input)); ok || len(paths) != 0 {
			t.Fatalf("invalid profile accepted: paths=%v", paths)
		}
	}
}

func TestReadLockContentionWritesMarker(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", state)
	key := sessionKey("contention-session")
	lock := filepath.Join(state, "context_io", "locks", key+".lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tryLock(f); err != nil {
		t.Fatal(err)
	}
	defer unlock(f)
	if err := Read(map[string]any{"session_id": "contention-session", "hook_event_name": "PreToolUse", "tool_name": "Read"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(state, "context_io", "contention", key)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("contention marker not written: %v", err)
	}
}

func TestSessionStartFailsOpenWhenAdvisorStateCannotBeWritten(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "init", "--quiet", repo)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	blockedStateRoot := filepath.Join(root, "state-is-a-file")
	if err := os.WriteFile(blockedStateRoot, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", blockedStateRoot)

	output := SessionStart(map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      "state-write-failure",
		"source":          "compact",
		"cwd":             repo,
	})
	want := `{"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": "Reply in the language the user writes in."}}`
	if string(output) != want {
		t.Fatalf("SessionStart output after state write failure = %q, want %q", output, want)
	}
}
