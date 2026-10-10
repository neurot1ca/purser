package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/pluginroot"
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

func TestActiveCoderStateReadValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want map[string]any
		bad  bool
	}{
		{name: "missing defaults", want: defaultActiveCoderState()},
		{name: "primary", data: `{"active_coder":"primary","triggering_evidence":null,"reset_cycle_marker":null,"last_probe_time":null}`, want: defaultActiveCoderState()},
		{name: "fallback", data: `{"active_coder":"fallback","triggering_evidence":{"type":"RATE_LIMITED","trigger_id":"r1"},"reset_cycle_marker":"cycle-1","last_probe_time":"2026-10-01T00:00:00Z"}`, want: map[string]any{"active_coder": "fallback", "triggering_evidence": map[string]any{"type": "RATE_LIMITED", "trigger_id": "r1"}, "reset_cycle_marker": "cycle-1", "last_probe_time": "2026-10-01T00:00:00Z"}},
		{name: "malformed json", data: `{`, bad: true},
		{name: "invalid active coder", data: `{"active_coder":"other","triggering_evidence":null,"reset_cycle_marker":null,"last_probe_time":null}`, bad: true},
		{name: "missing required field", data: `{"active_coder":"primary"}`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, root := t.TempDir(), t.TempDir()
			path := activeCoderPath(project, root)
			if tc.data != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readProjectActiveCoder(project, root)
			if (err != nil) != tc.bad {
				t.Fatalf("readProjectActiveCoder error = %v, want bad=%v", err, tc.bad)
			}
			if err == nil && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("state = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestP33ProcessWriter(t *testing.T) {
	if os.Getenv("P33_CHILD") != "1" {
		return
	}
	path := os.Getenv("P33_PATH")
	_, err := updateActiveCoderState(path, func(state map[string]any) {
		evidence := state["triggering_evidence"].(map[string]any)
		evidence["count"] = evidence["count"].(float64) + 1
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestP33ConcurrentProcessWritersSerialize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active-coder.json")
	if err := writeActiveCoderState(path, map[string]any{"active_coder": "primary", "triggering_evidence": map[string]any{"count": float64(0)}, "reset_cycle_marker": nil, "last_probe_time": nil}); err != nil {
		t.Fatal(err)
	}
	var commands []*exec.Cmd
	for i := 0; i < 8; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestP33ProcessWriter$")
		cmd.Env = append(os.Environ(), "P33_CHILD=1", "P33_PATH="+path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("writer: %v", err)
		}
	}
	got, err := readActiveCoderState(path)
	if err != nil || got["triggering_evidence"].(map[string]any)["count"] != float64(len(commands)) {
		t.Fatalf("serialized state = %#v, %v", got, err)
	}
}

func TestP33FailedReplacementKeepsOldFileAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active-coder.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	value := map[string]any{"active_coder": "fallback", "triggering_evidence": "new", "reset_cycle_marker": nil, "last_probe_time": nil}
	if err := writeActiveCoderState(path, value); err == nil {
		t.Fatal("replacement over directory succeeded")
	}
	if got, err := os.ReadFile(filepath.Join(path, "keep")); err != nil || string(got) != "old" {
		t.Fatalf("old destination changed: %q, %v", got, err)
	}
	if matches, _ := filepath.Glob(path + ".*.tmp"); len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestP34QueueFallbackQuestion(t *testing.T) {
	project := filepath.Join(t.TempDir(), "repo")
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	event := map[string]any{"session_id": "session-1"}
	if !queueFallbackQuestion(event, project, "question", "model-x", false) {
		t.Fatal("valid fallback question was not queued")
	}
	path := filepath.Join(root, "projects", repositoryProjectKey(project), "telemetry", "compaction_advisor.json")
	state := readAdvisorState(path, "session-1")
	pending := state["fallback_pending"].(map[string]any)
	if pending["line"] != "question" || pending["project"] != project || pending["model"] != "model-x" || pending["notice"] != false || pending["blocked"] != false || pending["delivered"] != false {
		t.Fatalf("queued fallback = %#v", pending)
	}
	pending["delivered"] = true
	state["fallback_pending"] = pending
	if err := writeAdvisorState(path, state); err != nil {
		t.Fatal(err)
	}
	if !queueFallbackQuestion(event, project, "question", "model-x", true) {
		t.Fatal("same fallback question was not replaced")
	}
	pending = readAdvisorState(path, "session-1")["fallback_pending"].(map[string]any)
	if pending["delivered"] != true || pending["notice"] != true {
		t.Fatalf("same question did not preserve delivery: %#v", pending)
	}
	if !queueFallbackQuestion(event, project, "new question", "model-y", false) {
		t.Fatal("changed fallback question was not queued")
	}
	pending = readAdvisorState(path, "session-1")["fallback_pending"].(map[string]any)
	if pending["delivered"] != false || pending["line"] != "new question" || pending["model"] != "model-y" {
		t.Fatalf("changed question retained stale delivery: %#v", pending)
	}
}

func TestP34QueueFallbackQuestionRejectsInvalidAndUndeliverableInputs(t *testing.T) {
	t.Run("missing repository", func(t *testing.T) {
		t.Setenv("PURSER_STATE_DIR", t.TempDir())
		if queueFallbackQuestion(map[string]any{"session_id": "session-1"}, "", "question", "model-x", false) {
			t.Fatal("empty repository accepted")
		}
	})
	t.Run("missing session", func(t *testing.T) {
		t.Setenv("PURSER_STATE_DIR", t.TempDir())
		if queueFallbackQuestion(map[string]any{}, filepath.Join(t.TempDir(), "repo"), "question", "model-x", false) {
			t.Fatal("missing session accepted")
		}
	})
	t.Run("write failure", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.WriteFile(root, []byte("file"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PURSER_STATE_DIR", root)
		if queueFallbackQuestion(map[string]any{"session_id": "session-1"}, filepath.Join(t.TempDir(), "repo"), "question", "model-x", false) {
			t.Fatal("unwritable state accepted")
		}
	})
}

func TestP34FallbackQuestionShownThenAnswerable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	config := filepath.Join(home, ".orchestrator")
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.toml"), []byte("[compaction]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "--quiet", project).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	event := map[string]any{"session_id": "session-1", "cwd": project, "prompt": "Continue."}
	question := "> ⚓ **Should Purser switch to `model-x`?**"
	path := activeCoderPath(project, root)
	if err := writeActiveCoderState(path, map[string]any{
		"active_coder": "primary", "triggering_evidence": map[string]any{
			"question_session": "session-1", "question_shown": false, "answerable": false,
		}, "reset_cycle_marker": nil, "last_probe_time": nil,
	}); err != nil {
		t.Fatal(err)
	}
	if !queueFallbackQuestion(event, project, question, "model-x", false) {
		t.Fatal("fallback question was not queued")
	}
	first := UserPromptSubmit(event, time.Now())
	if !strings.Contains(string(first), question) {
		t.Fatalf("disabled-advice prompt omitted fallback question: %s", first)
	}
	value, err := readActiveCoderState(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := value["triggering_evidence"].(map[string]any)
	if evidence["question_shown"] != false || evidence["answerable"] != false {
		t.Fatalf("queued question became answerable before it was shown: %#v", evidence)
	}
	markFallbackShown(project, "other-session")
	markFallbackAnswerable(project, "other-session")
	value, err = readActiveCoderState(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence = value["triggering_evidence"].(map[string]any)
	if evidence["question_shown"] != false || evidence["answerable"] != false {
		t.Fatalf("other session changed fallback question: %#v", evidence)
	}
	stopEvent := map[string]any{"session_id": "session-1", "cwd": project, "last_assistant_message": question}
	if stop := Stop(stopEvent, time.Now()); len(stop) != 0 {
		t.Fatalf("displayed question unexpectedly blocked: %s", stop)
	}
	value, err = readActiveCoderState(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence = value["triggering_evidence"].(map[string]any)
	if evidence["question_shown"] != true || evidence["answerable"] != false {
		t.Fatalf("shown question lifecycle = %#v", evidence)
	}
	event["prompt"] = "Yes."
	UserPromptSubmit(event, time.Now())
	value, err = readActiveCoderState(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence = value["triggering_evidence"].(map[string]any)
	if evidence["question_shown"] != true || evidence["answerable"] != true {
		t.Fatalf("next-turn question lifecycle = %#v", evidence)
	}
}

func TestContextTurnEventRecordsGoRuntime(t *testing.T) {
	event := contextTurnEvent(newTurn(""), "session-1", 0, 0, time.Now())
	payload, ok := event["payload"].(map[string]any)
	if !ok || payload["runtime"] != "go" {
		t.Fatalf("context turn runtime = %v, want go", payload["runtime"])
	}
	if event["session_id"] != "session-1" {
		t.Fatalf("context turn session_id = %v, want session-1", event["session_id"])
	}
}

func TestPersistContextTurnRoutesEmptyRawSessionIDAfterNormalization(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", root)
	event := map[string]any{
		"schema_version": 1,
		"event_id":       "00000000-0000-4000-8000-000000000001",
		"ts_utc":         "2026-10-03T12:00:00Z",
		"event_type":     "CONTEXT_IO_TURN_RECORDED",
		"task_id":        nil,
		"session_id":     "",
	}
	if !persistContextTurn(event) {
		t.Fatal("persistContextTurn failed")
	}
	path := filepath.Join(root, "telemetry", "events", "2026-10", "session_session-e3b0c442.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("normalized session event missing at %s: %v", path, err)
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
	usernameHome := current.HomeDir
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
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	pluginCommand := filepath.Join(pluginRoot, "bin", "purser-hook-"+runtime.GOOS+"-"+runtime.GOARCH+suffix)
	projectCommand := filepath.Join(projectRoot, "bin", "purser-hook-"+runtime.GOOS+"-"+runtime.GOARCH+suffix)
	state := map[string]any{"maintenance_pending": map[string]any{
		"notice_file": filepath.Join(projectRoot, "maintenance", "consent.json"),
		"text":        "> ⚓ **Run it?**",
	}}
	got := lifecycleGuidance(state)
	if !strings.Contains(got, `"`+pluginCommand+`" maintenance`) {
		t.Fatalf("maintenance CLI did not use plugin command path %q: %s", pluginCommand, got)
	}
	if strings.Contains(got, projectCommand) {
		t.Fatalf("maintenance CLI used project command path %q: %s", projectCommand, got)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	fallback := filepath.Join(pluginroot.Root(), "bin", "purser-hook-"+runtime.GOOS+"-"+runtime.GOARCH+suffix)
	if got := maintenanceCommandPath(); filepath.Clean(got) != filepath.Clean(fallback) {
		t.Fatalf("maintenance command fallback = %q, want %q", got, fallback)
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

func TestCompactionAdviceUsesPostBoundaryUsage(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	writeAdvisorTranscript(t, transcript, []map[string]any{
		{"type": "assistant", "message": map[string]any{"usage": map[string]any{"input_tokens": 319608}}},
		{"type": "system", "subtype": "compact_boundary"},
	})
	statePath := filepath.Join(t.TempDir(), "telemetry", "compaction_advisor.json")
	got := compactionAdvice(map[string]any{"transcript_path": transcript}, t.TempDir(), statePath, "post-compact-small")
	if got != "" {
		t.Fatalf("advice after compact boundary without post-boundary assistant usage = %q, want none", got)
	}
}

func TestCompactionAdviceThresholdUsesPostBoundaryUsage(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	writeAdvisorTranscript(t, transcript, []map[string]any{
		{"type": "assistant", "message": map[string]any{"usage": map[string]any{"input_tokens": 319608}}},
		{"type": "system", "subtype": "compact_boundary"},
		{"type": "assistant", "message": map[string]any{"usage": map[string]any{"input_tokens": 287321}}},
	})
	statePath := filepath.Join(t.TempDir(), "telemetry", "compaction_advisor.json")
	got := compactionAdvice(map[string]any{"transcript_path": transcript}, t.TempDir(), statePath, "post-compact-large")
	if !strings.Contains(got, "287,321 context tokens") {
		t.Fatalf("advice after compact boundary did not use 287,321 post-boundary tokens: %q", got)
	}
}

func TestCompactionAdviceResolvesPendingOutcomeFromNewBoundary(t *testing.T) {
	t.Setenv("USERPROFILE", t.TempDir())
	stateRoot := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	records := make([]map[string]any, 0, 482)
	for i := 0; i < 469; i++ {
		records = append(records, map[string]any{"type": "user", "message": map[string]any{"content": "prompt"}})
	}
	records = append(records, map[string]any{"type": "system", "subtype": "compact_boundary"})
	for i := 0; i < 12; i++ {
		records = append(records, map[string]any{"type": "user", "message": map[string]any{"content": "prompt"}})
	}
	records = append(records, map[string]any{"type": "assistant", "message": map[string]any{"usage": map[string]any{"input_tokens": 74714}}})
	writeAdvisorTranscript(t, transcript, records)
	statePath := filepath.Join(t.TempDir(), "telemetry", "compaction_advisor.json")
	state := map[string]any{
		"session_id": "post-compact-pending", "context_prompt_count": 481,
		"last_suggested_context": 319608,
		"notice_pending": map[string]any{
			"notice_id": "05c3c9ce0123456789abcdef01234567", "context": 319608,
			"percentage": 77, "handoff": "handoff", "triggers": []any{"idle", "commit"},
			"advice_written": true, "advice_turn": 469,
		},
		"compaction_outcome_pending": map[string]any{
			"advice_turn": 469, "compaction_observed": false,
			"notice_id": "05c3c9ce0123456789abcdef01234567", "pre_context_tokens": 319608,
		},
	}
	if err := writeAdvisorState(statePath, state); err != nil {
		t.Fatal(err)
	}
	got := compactionAdvice(map[string]any{"transcript_path": transcript}, t.TempDir(), statePath, "post-compact-pending")
	if got != "" {
		t.Fatalf("stale advice was replayed after compact boundary: %q", got)
	}
	updated := readAdvisorState(statePath, "post-compact-pending")
	if updated["compaction_outcome_pending"] != nil || updated["notice_pending"] != nil {
		t.Fatalf("compaction state was not resolved: outcome=%#v notice=%#v", updated["compaction_outcome_pending"], updated["notice_pending"])
	}
}

func writeAdvisorTranscript(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	var data []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
