package enforce

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/lease"
	"purser-hook/internal/ledger"
)

func TestAppendPreCodeMapScopedExcerpt(t *testing.T) {
	mapPath := filepath.Join(t.TempDir(), "map.txt")
	content := "# Code map v5\nParser: native\nCovered languages: go\n\n" +
		"pkg/a.go\n  1 function A()\n\n" +
		"pkg/nested/b.go\n  2 function B()\n\n" +
		"pkg-other/c.go\n  3 function C()\n"
	if err := os.WriteFile(mapPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, scope, want, notWant string
	}{
		{name: "directory", scope: "pkg", want: "pkg/nested/b.go", notWant: "pkg-other/c.go"},
		{name: "file", scope: "pkg/a.go", want: "pkg/a.go", notWant: "pkg/nested/b.go"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := appendPreCodeMap("task", mapPath, "FRESH", []string{test.scope})
			if !strings.Contains(got, "CODE_MAP="+mapPath+" STATUS=FRESH (scoped excerpt below)\n") || !strings.Contains(got, test.want) || strings.Contains(got, test.notWant) {
				t.Fatalf("unexpected scoped prompt:\n%s", got)
			}
		})
	}
}

func TestAppendPreCodeMapExcerptCapKeepsWholeSections(t *testing.T) {
	mapPath := filepath.Join(t.TempDir(), "map.txt")
	large := strings.Repeat("x", 3200)
	content := "# Code map v5\nParser: native\nCovered languages: go\n\na.go\n  " + large +
		"\n\nb.go\n  " + large + "\n\nc.go\n  " + large + "\n"
	if err := os.WriteFile(mapPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	got := appendPreCodeMap("task", mapPath, "STALE", []string{"."})
	if len([]byte(got[strings.Index(got, "CODE_MAP="):])) > preCodeMapExcerptMaxBytes {
		t.Fatalf("excerpt exceeds %d bytes", preCodeMapExcerptMaxBytes)
	}
	if !strings.Contains(got, "a.go\n  "+large+"\n... (2 more files in "+mapPath+")") || strings.Contains(got, "b.go") {
		t.Fatalf("excerpt did not retain whole sections and report omitted files:\n%s", got)
	}
}

func TestAppendPreCodeMapFallsBackToPointer(t *testing.T) {
	mapPath := filepath.Join(t.TempDir(), "map.txt")
	if err := os.WriteFile(mapPath, []byte("# Code map\n\nother.go\n  1 function Other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, status string
		scope        []string
	}{
		{name: "no scope", status: "FRESH"},
		{name: "no matching sections", status: "FRESH", scope: []string{"missing"}},
		{name: "unavailable", status: "UNAVAILABLE", scope: []string{"."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := appendPreCodeMap("task", mapPath, test.status, test.scope)
			want := "task\nCODE_MAP=" + mapPath + " STATUS=" + test.status
			if got != want {
				t.Fatalf("got %q want %q", got, want)
			}
		})
	}
}

func TestAppendPrePlanShapeIsPlanOnlyAndIdempotent(t *testing.T) {
	block := appendPrePlanShape("task", "plan")
	if strings.Count(block, prePlanShapeMarker) != 1 || !strings.Contains(block, "Summary:") || !strings.Contains(block, "Milestones") {
		t.Fatalf("plan shape block missing or duplicated:\n%s", block)
	}
	if got := appendPrePlanShape(block, "plan"); got != block {
		t.Fatalf("existing plan shape block changed or duplicated:\n%s", got)
	}
	if got := appendPrePlanShape("implementation", "implementation"); strings.Contains(got, prePlanShapeMarker) {
		t.Fatalf("implementation prompt received plan shape:\n%s", got)
	}
}

func TestPlanMilestoneLabel(t *testing.T) {
	for _, test := range []struct {
		name, taskID, label, want string
	}{
		{name: "plan milestone", taskID: "ci-go-plan-m5of5", label: "CI green on 3 OSes", want: "CI green on 3 OSes [ci-go-plan-5/5]"},
		{name: "non-plan", taskID: "ci-go", label: "CI green on 3 OSes", want: "CI green on 3 OSes"},
		{name: "already suffixed", taskID: "ci-go-plan-m5of5", label: "CI green on 3 OSes [ci-go-plan-5/5]", want: "CI green on 3 OSes [ci-go-plan-5/5]"},
		{name: "empty", taskID: "ci-go-plan-m5of5", label: "", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := planMilestoneLabel(test.taskID, test.label); got != test.want {
				t.Fatalf("planMilestoneLabel(%q, %q) = %q, want %q", test.taskID, test.label, got, test.want)
			}
		})
	}
}

func TestPreDecisionsP63Fixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/pre_decisions.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string         `json:"name"`
			Tool     string         `json:"tool"`
			Input    map[string]any `json:"input"`
			State    string         `json:"state"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&fixture); err != nil || fixture.Oracle != "Python codex.pre_tool_use" {
		t.Fatalf("fixture decode: %v %q", err, fixture.Oracle)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			if item.Expected == nil {
				t.Fatal("Python oracle fixture has no expected decision")
			}
			scratch := preTempDir(t)
			repo := filepath.Join(scratch, "repo")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Claude runs the full suite once before acceptance.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			home, runtimeRoot := filepath.Join(scratch, "home"), filepath.Join(scratch, "state")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOMEDRIVE", "")
			t.Setenv("HOMEPATH", "")
			pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PURSER_STATE_DIR", runtimeRoot)
			t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			input := replaceRepo(item.Input, repo)
			if item.State == "thread" {
				seedPreThread(t, repo, runtimeRoot)
			}
			if item.State == "fallback" {
				if err := os.MkdirAll(filepath.Join(home, ".orchestrator"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".orchestrator", "config.toml"), []byte("[fallback]\nagent = \"agy\"\nmodel = \"gemini-3.8-flash-high\"\nfamily = \"google\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				seedFallback(t, repo, runtimeRoot)
			}
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": item.Tool, "tool_input": input,
				"session_id": "session-p63", "tool_use_id": "use-p63"}
			got, gotErr := Pre(event, runtimeRoot)
			if item.Tool == "mcp__codex-main__codex_start" {
				output := mapValue(got["hookSpecificOutput"])
				if gotErr != nil || mapValue(output["updatedInput"]) == nil || output["permissionDecision"] != nil {
					t.Fatalf("async start was not durably admitted: %#v %v", got, gotErr)
				}
				pending, _ := pendingPath(runtimeRoot, event)
				if raw, readErr := os.ReadFile(pending); readErr != nil || !strings.Contains(string(raw), `"async_ready":true`) || strings.Contains(string(raw), `"dispatcherAgentId"`) {
					t.Fatalf("async start pending record is not ready: %v %s", readErr, raw)
				}
				event["agent_id"] = "dispatcher-1"
				if _, err := Pre(event, runtimeRoot); err != nil {
					t.Fatal(err)
				}
				raw, readErr := os.ReadFile(pending)
				var saved map[string]any
				if readErr != nil || json.Unmarshal(raw, &saved) != nil || saved["dispatcherAgentId"] != "dispatcher-1" {
					t.Fatalf("subagent start pending record lost dispatcher identity: %v %s", readErr, raw)
				}
				return
			}
			want := replaceFixtureProjectKey(item.Expected, projectKey(&repo))
			gotNormalized := replaceTemporaryRoot(got, scratch)
			gotJSON, _ := canonicalJSON(gotNormalized)
			wantJSON, _ := canonicalJSON(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("Pre decision mismatch\n got: %s\nwant: %s\nerr: %v", gotJSON, wantJSON, gotErr)
			}
		})
	}
}

func TestPreTelemetryScopePathCount(t *testing.T) {
	scratch := preTempDir(t)
	repo, runtimeRoot := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	home, _ := filepath.Abs(filepath.Join(scratch, "home"))
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PURSER_STATE_DIR", runtimeRoot)
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
	for _, tc := range []struct {
		name, phase, route, scope string
		wantCount                 float64
	}{
		{name: "implementation", phase: "implementation", route: "normal", scope: "src/a.go,src/b.go", wantCount: 2},
		{name: "operation", phase: "operation", route: "operation", wantCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := []string{"task=" + tc.name, "phase=" + tc.phase, "route=" + tc.route}
			if tc.route == "normal" {
				fields = append(fields, "reason=new_task", "basis=specific boundary", "scope="+tc.scope)
			}
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__codex-main__codex", "tool_use_id": "scope-count-call",
				"session_id": "scope-count-session", "cwd": repo, "tool_input": map[string]any{"prompt": "ORCH|" + strings.Join(fields, "|") + "\nImplement.", "label": "scope count"}}
			if _, err := Pre(event, runtimeRoot); err != nil {
				t.Fatalf("Pre: %v", err)
			}
			path := filepath.Join(runtimeRoot, "telemetry", "events", "by_task", tc.name+".jsonl")
			file, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			seen := []string{}
			for _, line := range bytes.Split(bytes.TrimSpace(file), []byte{'\n'}) {
				var observation map[string]any
				if err := json.Unmarshal(line, &observation); err != nil {
					t.Fatal(err)
				}
				if observation["event_type"] != "TASK_ROUTE_SELECTED" && observation["event_type"] != "IMPLEMENTER_STARTED" {
					continue
				}
				payload := mapValue(observation["payload"])
				if payload["scope_path_count"] != tc.wantCount || payload["scope"] != nil {
					t.Fatalf("unexpected scope telemetry: %#v", payload)
				}
				seen = append(seen, observation["event_type"].(string))
			}
			if strings.Join(seen, ",") != "TASK_ROUTE_SELECTED,IMPLEMENTER_STARTED" {
				t.Fatalf("scope event types = %#v", seen)
			}
		})
	}
}

func TestPreTelemetryEnvelopeIdentityAndRepositoryBranch(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/feature/telemetry\n"), 0600); err != nil {
		t.Fatal(err)
	}
	preEmit(root, map[string]any{"session_id": "session", "tool_use_id": "tool-call-id", "agent_id": "agent-7", "agent_type": "reviewer"}, &repo,
		"task", "unresolved", "TASK_ROUTE_SELECTED", map[string]any{"phase": "implementation", "route": "normal", "model": "gpt-6-sol", "effort": "medium", "routing_enforced": true, "fresh_thread": true, "reply_call": false})
	path := filepath.Join(root, "telemetry", "events", "by_task", "task.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
		t.Fatal(err)
	}
	if event["session_id"] != "session" || event["repo"] != filepath.Base(repo) || event["branch"] != normalizeObservationID("feature/telemetry", "branch") || event["agent_id"] != "agent-7" || event["agent_type"] != "reviewer" {
		t.Fatalf("observation envelope = %#v", event)
	}
	if strings.Contains(string(data), "tool-call-id") {
		t.Fatalf("tool call id leaked into observation: %s", data)
	}
}

func TestPreTelemetryDetachedOrUnreadableBranchIsNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		setup func(string) error
	}{
		{name: "detached", setup: func(path string) error { return os.WriteFile(path, []byte("1e85bd1f4fa2d73c1ae011388c3504663a892db5\n"), 0600) }},
		{name: "unreadable", setup: func(path string) error { return os.Mkdir(path, 0700) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := tc.setup(filepath.Join(repo, ".git", "HEAD")); err != nil {
				t.Fatal(err)
			}
			preEmit(root, map[string]any{"session_id": "session"}, &repo, "task", "unresolved", "TASK_ROUTE_SELECTED",
				map[string]any{"phase": "implementation", "route": "normal", "model": "gpt-6-sol", "effort": "medium", "routing_enforced": true, "fresh_thread": true, "reply_call": false})
			data, err := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "task.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
				t.Fatal(err)
			}
			if event["branch"] != nil {
				t.Fatalf("branch = %#v, want null", event["branch"])
			}
		})
	}
}

func TestPreWorktreeReplyQueuesOnlyOnHotOverlap(t *testing.T) {
	for _, tc := range []struct {
		name, scope string
		wantQueued  bool
	}{
		{name: "overlapping active lease", scope: "tests/", wantQueued: true},
		{name: "non-overlapping reply", scope: "docs/", wantQueued: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := preTempDir(t)
			repo, runtimeRoot := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			home := filepath.Join(scratch, "home")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("PURSER_STATE_DIR", runtimeRoot)
			t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			thread := "worktree-reply"
			project := projectKey(&repo)
			prior := ledger.BuildSessionRecord("reply-task", &thread, "normal", "gpt-6.1-sol", "medium", map[string]any{
				"phase": "implementation", "scope": []any{"tests/"}, "label": "worktree reply",
			})
			if saved := ledger.Save(filepath.Join(runtimeRoot, "projects", project), prior); saved.Status != ledger.OK {
				t.Fatalf("seed thread session: %#v", saved)
			}
			shimRoot := filepath.Join(runtimeRoot, "codex_exec_mcp", "threads")
			threadSum := sha256.Sum256([]byte(thread))
			if err := atomicAdmissionJSON(filepath.Join(shimRoot, hex.EncodeToString(threadSum[:])+".json"), map[string]any{
				"thread_id": thread, "cwd": repo, "config_args": []any{}, "worktreePath": repo,
			}); err != nil {
				t.Fatal(err)
			}
			store := lease.New(filepath.Join(runtimeRoot, "projects", project))
			now := float64(time.Now().UnixNano()) / 1e9
			if got := store.Acquire("active-owner", "other-task", []string{"tests/"}, lease.AcquireOptions{}, now); got.Status != "OK" {
				t.Fatalf("seed active lease: %#v", got)
			}
			if err := store.RecordConflicts([]string{"tests/hot.py"}, now); err != nil {
				t.Fatal(err)
			}
			prompt := "ORCH|task=reply-task|phase=implementation|route=normal|reason=continue|basis=continue implementation|scope=" + tc.scope + "\nContinue."
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__codex-main__codex_start",
				"tool_use_id": "worktree-reply-" + tc.name, "session_id": "worktree-reply-session", "cwd": repo,
				"tool_input": map[string]any{"mode": "reply", "threadId": thread, "prompt": prompt, "label": "worktree reply"}}
			got, err := Pre(event, runtimeRoot)
			if err != nil {
				t.Fatalf("Pre: %v", err)
			}
			updated := mapValue(mapValue(got["hookSpecificOutput"])["updatedInput"])
			if updated == nil {
				t.Fatalf("async reply was not admitted: %#v", got)
			}
			if queued := updated["leaseQueued"] == true; queued != tc.wantQueued {
				t.Fatalf("leaseQueued=%v, want %v; input=%#v", queued, tc.wantQueued, updated)
			}
		})
	}
}

func TestPreLatestModelDispatchAndReplies(t *testing.T) {
	for _, liveModel := range []string{"", "gpt-6.1-sol", "gpt-6.3-sol", "gpt-6.2-luna"} {
		t.Run("live="+liveModel, func(t *testing.T) {
			root := preTempDir(t)
			repo, state := filepath.Join(root, "repo"), filepath.Join(root, "state")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			t.Setenv("HOME", root)
			t.Setenv("USERPROFILE", root)
			t.Setenv("PURSER_STATE_DIR", state)
			t.Setenv("CODEX_HOME", root)
			t.Setenv("CLAUDE_PLUGIN_ROOT", root)
			data, err := os.ReadFile("../../../.claude/hooks/orchestrator/config.json")
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			floor := "gpt-6.1-sol"
			if liveModel != "" {
				floor = "gpt-6.2-sol"
			}
			mapValue(mapValue(config["routes"])["normal"])["model"] = floor
			data, _ = json.Marshal(config)
			path := filepath.Join(root, ".claude", "hooks", "orchestrator", "config.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-6.2-sol","visibility":"list"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			tool := "mcp__codex-main__codex"
			input := map[string]any{"prompt": "ORCH|task=task-recorded|phase=implementation|route=normal|reason=new_task|basis=model resolution|scope=src/a.go\nImplement.", "label": "latest model", "cwd": repo}
			want := "gpt-6.2-sol"
			if liveModel != "" {
				seedPreThread(t, repo, state)
				thread := "thread-p63"
				record := ledger.BuildSessionRecord("task-recorded", &thread, "normal", liveModel, "medium", map[string]any{"phase": "implementation", "scope": []any{"src/a.go"}, "label": "latest model"})
				if saved := ledger.Save(filepath.Join(state, "projects", projectKey(&repo)), record); saved.Status != ledger.OK {
					t.Fatal(saved)
				}
				tool, input["threadId"], want = "mcp__codex-main__codex-reply", thread, liveModel
			}
			got, err := Pre(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input, "cwd": repo, "session_id": "latest", "tool_use_id": "latest"}, state)
			if err != nil {
				t.Fatal(err)
			}
			output := mapValue(got["hookSpecificOutput"])
			if liveModel == "gpt-6.2-luna" {
				if !strings.HasPrefix(stringValue(output["permissionDecisionReason"]), "MODEL_MISMATCH:") {
					t.Fatalf("different family admitted: %#v", got)
				}
			} else if output["permissionDecision"] == "deny" || !strings.Contains(stringValue(got["systemMessage"]), "-> "+want+" / ") ||
				(liveModel == "" && mapValue(output["updatedInput"])["model"] != want) {
				t.Fatalf("wanted model %s, got %#v", want, got)
			}
		})
	}
}

func TestPreCodexStartRejectsFinishedTaskID(t *testing.T) {
	scratch := preTempDir(t)
	repo, runtimeRoot := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	home := filepath.Join(scratch, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	pluginRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")
	t.Setenv("PURSER_STATE_DIR", runtimeRoot)
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
	thread := "finished-thread"
	record := ledger.BuildSessionRecord("task-finished", &thread, "normal", "gpt-6-sol", "medium", map[string]any{
		"phase": "implementation", "scope": []any{"src/a.go"}, "label": "finished task",
	})
	record["last_outcome"] = map[string]any{"codex_quota": map[string]any{"remaining": 27.0, "limit": 60.0}}
	if saved := ledger.Save(filepath.Join(runtimeRoot, "projects", projectKey(&repo)), record); saved.Status != ledger.OK {
		t.Fatalf("seed finished task: %#v", saved)
	}
	prompt := "ORCH|task=task-finished|phase=implementation|route=normal|reason=new_task|basis=specific boundary|scope=src/a.go\nContinue."
	event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__codex-main__codex_start",
		"tool_use_id": "finished-start", "session_id": "finished-session", "cwd": repo,
		"tool_input": map[string]any{"mode": "new", "prompt": prompt, "label": "finished task"}}
	got, gotErr := Pre(event, runtimeRoot)
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	output := mapValue(got["hookSpecificOutput"])
	if output["permissionDecision"] != "deny" || stringValue(output["permissionDecisionReason"]) !=
		"TASK_ALREADY_FINISHED: This task already finished; use a fresh task id." {
		t.Fatalf("finished task denial: %#v", output)
	}
}

func TestPreInvalidOrchWithoutTask(t *testing.T) {
	scratch := preTempDir(t)
	plugin, _ := filepath.Abs("../../..")
	for key, value := range map[string]string{"HOME": scratch, "USERPROFILE": scratch, "HOMEDRIVE": "", "HOMEPATH": "", "CLAUDE_PLUGIN_ROOT": plugin} {
		t.Setenv(key, value)
	}
	event := map[string]any{"tool_name": "mcp__codex-main__codex_start", "tool_input": map[string]any{
		"mode": "new", "prompt": "ORCH|phase=implementation|route=subtle|reason=new_task|basis=design boundary|scope=src/a.go\nImplement."}}
	got, err := preDecision(event, scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	hook := mapValue(got["hookSpecificOutput"])
	if hook["permissionDecision"] != "deny" || !strings.HasPrefix(stringValue(hook["permissionDecisionReason"]), "MISSING_REQUIRED_KEY:") {
		t.Fatalf("expected normal parse denial: %#v", got)
	}
}

func TestPreTaskRetryDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, disposition, category, route, model, effort, mode, deny string
		finished, corrupt, sync, duplicate                            bool
	}{
		{name: "first", route: "subtle"},
		{name: "luna override", category: "rejected_after_sendback", route: "subtle", model: "gpt-6-luna", effort: "low"},
		{name: "consequential floor", category: "rejected_after_sendback", route: "consequential", model: "gpt-6-sol", effort: "medium"},
		{name: "escalated floor", category: "rejected_after_sendback", route: "escalated", model: "gpt-6.1-sol", effort: "high"},
		{name: "corrupt dispatch", route: "subtle", corrupt: true},
		{name: "corrupt reuse", route: "subtle", corrupt: true, finished: true, deny: "TASK_ALREADY_FINISHED:"},
		{name: "cancelled reuse", route: "subtle", disposition: "cancelled", finished: true},
		{name: "discarded reuse", route: "subtle", disposition: "discarded", finished: true},
		{name: "failed reuse", route: "subtle", disposition: "failed", category: "failed_abandonment", finished: true},
		{name: "started protected", route: "subtle", disposition: "started", finished: true, deny: "TASK_ALREADY_FINISHED:"},
		{name: "accepted protected", route: "subtle", disposition: "accepted", finished: true, deny: "TASK_ALREADY_FINISHED:"},
		{name: "applied protected", route: "subtle", disposition: "applied", finished: true, deny: "TASK_ALREADY_FINISHED:"},
		{name: "unresolved protected", route: "subtle", disposition: "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", finished: true, deny: "TASK_ALREADY_FINISHED:"},
		{name: "async reply fresh", route: "consequential", category: "rejected_after_sendback", mode: "reply"},
		{name: "sync reply denied", route: "subtle", category: "rejected_after_sendback", mode: "reply", sync: true, deny: "Start a fresh job for this task with route=consequential and basis=prior luna attempt failed after send-back."},
		{name: "duplicate replay", route: "subtle", disposition: "applied", finished: true, duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := preTempDir(t)
			repo, state := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
			if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			plugin, _ := filepath.Abs("../../..")
			for key, value := range map[string]string{"HOME": scratch, "USERPROFILE": scratch, "HOMEDRIVE": "", "HOMEPATH": "", "PURSER_STATE_DIR": state, "CLAUDE_PLUGIN_ROOT": plugin, "CODEX_HOME": scratch} {
				t.Setenv(key, value)
			}
			if tc.finished || tc.mode == "reply" {
				seedPreThread(t, repo, state)
				if tc.finished {
					path, _ := ledger.PathForTask(filepath.Join(state, "projects", projectKey(&repo)), "task-recorded")
					record := ledger.Load(path, "task-recorded").Record
					record["last_outcome"] = map[string]any{"status": "finished"}
					if saved := ledger.Save(filepath.Dir(filepath.Dir(path)), record); saved.Status != ledger.OK {
						t.Fatal(saved)
					}
				}
			}
			attempt := TaskAttempt{ProjectKey: projectKey(&repo), TaskID: "task-recorded", LatestJobID: "prior-job", TerminalDisposition: tc.disposition}
			if tc.category != "" {
				attempt.Failure = &TaskFailureEvidence{JobID: "prior-job", Category: tc.category}
			}
			if tc.category != "" || tc.disposition != "" || tc.corrupt {
				if err := SaveTaskAttempt(state, attempt); err != nil {
					t.Fatal(err)
				}
			}
			if tc.corrupt {
				if err := os.WriteFile(taskAttemptPath(state, attempt.ProjectKey, attempt.TaskID), []byte(`{"terminal_disposition":"cancelled","failure":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			input := map[string]any{"mode": "new", "cwd": repo, "label": "retry task", "prompt": "ORCH|task=task-recorded|phase=implementation|route=" + tc.route + "|reason=new_task|basis=design boundary|scope=src/a.go|eta=7m\nImplement."}
			tool := "mcp__codex-main__codex_start"
			if tc.mode == "reply" {
				input["mode"], input["threadId"], input["cwd"] = "reply", "thread-p63", scratch
			}
			if tc.sync {
				tool = "mcp__codex-main__codex-reply"
			}
			if tc.model != "" {
				input["model"], input["config"] = tc.model, map[string]any{"model_reasoning_effort": tc.effort}
			}
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input, "cwd": repo, "session_id": "retry", "tool_use_id": "retry"}
			key, _ := callKey(event)
			call := codexCallUUID(key)
			if tc.duplicate {
				digest := sha256.Sum256([]byte(call))
				path := filepath.Join(state, "codex_exec_mcp", "jobs", hex.EncodeToString(digest[:])+".json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				data, _ := json.Marshal(map[string]any{"jobId": hex.EncodeToString(digest[:]), "state": "SUCCEEDED"})
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Pre(event, state)
			if err != nil {
				t.Fatal(err)
			}
			hook := mapValue(got["hookSpecificOutput"])
			if tc.corrupt {
				data, _ := os.ReadFile(filepath.Join(state, "telemetry", "events", "by_task", "task-recorded.jsonl"))
				if bytes.Count(data, []byte(`"code":"TASK_ATTEMPT_UNREADABLE"`)) != 1 {
					t.Fatalf("corrupt attempt observation: %s", data)
				}
			}
			if tc.duplicate {
				if hook["permissionDecision"] == "deny" || mapValue(hook["updatedInput"])["call"] != call {
					t.Fatalf("duplicate denied: %#v", got)
				}
				return
			}
			if tc.deny != "" {
				text := stringValue(hook["permissionDecisionReason"])
				if hook["permissionDecision"] != "deny" || !strings.HasPrefix(text, tc.deny) || (tc.sync && text != tc.deny) {
					t.Fatalf("denial: %#v", got)
				}
				return
			}
			updated := mapValue(hook["updatedInput"])
			wantRoute, wantModel, wantEffort := tc.route, "gpt-6-luna", "high"
			if tc.category != "" {
				wantModel = "gpt-6.1-sol"
				if tc.route == "escalated" {
					wantEffort = "xhigh"
				} else {
					wantRoute = "consequential"
				}
				basis := "Prior luna implementation attempt prior-job was rejected after send-back."
				if tc.category == "failed_abandonment" {
					basis = "Prior luna implementation attempt prior-job was cancelled as failed."
				}
				header := "ORCH|task=task-recorded|phase=implementation|route=" + wantRoute + "|reason=repeated_failure|basis=" + basis + "|scope=src/a.go|eta=7m\nImplement."
				if !strings.HasPrefix(stringValue(updated["prompt"]), header) || got["systemMessage"] != "Purser: prior luna attempt failed; route="+wantRoute+" -> "+wantModel+" / "+wantEffort+"." {
					t.Fatalf("rewritten header/receipt: %#v", got)
				}
			}
			if hook["permissionDecision"] == "deny" || updated["route"] != wantRoute || updated["jobModel"] != wantModel || updated["jobEffort"] != wantEffort || updated["mode"] != "new" || updated["model"] != wantModel || mapValue(updated["config"])["model_reasoning_effort"] != wantEffort {
				t.Fatalf("effective dispatch: %#v", got)
			}
			if tc.mode == "reply" && (updated["threadId"] != nil || updated["cwd"] != repo) {
				t.Fatalf("fresh reply retained thread/cwd: %#v", updated)
			}
			path, _ := pendingPath(state, event)
			data, _ := os.ReadFile(path)
			var pending map[string]any
			if json.Unmarshal(data, &pending) != nil || pending["route"] != wantRoute || pending["model"] != wantModel || pending["effort"] != wantEffort {
				t.Fatalf("pending dispatch: %s", data)
			}
		})
	}
}

func replaceFixtureProjectKey(value any, project string) any {
	switch value := value.(type) {
	case string:
		return strings.ReplaceAll(value, "__PROJECT_KEY__", project)
	case map[string]any:
		for key, item := range value {
			value[key] = replaceFixtureProjectKey(item, project)
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = replaceFixtureProjectKey(item, project)
		}
		return value
	default:
		return value
	}
}

func replaceRepo(input map[string]any, repo string) map[string]any {
	copy := make(map[string]any, len(input))
	for key, value := range input {
		if value == "__REPO__" {
			value = repo
		}
		copy[key] = value
	}
	return copy
}
func replaceTemporaryRoot(value any, scratch string) any {
	switch value := value.(type) {
	case string:
		value = strings.ReplaceAll(value, scratch, "__TMP_ROOT__")
		if strings.Contains(value, "__TMP_ROOT__") {
			value = strings.ReplaceAll(value, `\`, "/")
		}
		return value
	case map[string]any:
		for key, item := range value {
			value[key] = replaceTemporaryRoot(item, scratch)
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = replaceTemporaryRoot(item, scratch)
		}
		return value
	default:
		return value
	}
}
func seedPreThread(t *testing.T, repo, runtimeRoot string) {
	t.Helper()
	project := filepath.Join(runtimeRoot, "projects", projectKey(&repo))
	thread := "thread-p63"
	record := ledger.BuildSessionRecord("task-recorded", &thread, "subtle", "gpt-6-luna", "high", map[string]any{
		"phase": "implementation", "scope": []any{"src/a.go"}, "label": "saved label", "task_contract_grounded": false,
	})
	if result := ledger.Save(project, record); result.Status != ledger.OK {
		t.Fatalf("seed ledger: %#v", result)
	}
	sum := sha256.Sum256([]byte(thread))
	path := filepath.Join(runtimeRoot, "threads", hex.EncodeToString(sum[:])+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(map[string]any{"thread_id": thread, "project_root": repo})
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}
func seedFallback(t *testing.T, repo, runtimeRoot string) {
	t.Helper()
	project := filepath.Join(runtimeRoot, "projects", projectKey(&repo))
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"active_coder": "fallback", "triggering_evidence": map[string]any{"type": "RATE_LIMITED", "trigger_id": "rate-1", "switched_at": "99999999999", "retryAfterSeconds": 3600}, "reset_cycle_marker": nil, "last_probe_time": nil}
	encoded, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(project, "active-coder.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}
