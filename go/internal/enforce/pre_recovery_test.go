package enforce

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"strings"
	"testing"
)

func TestPreRecoveryP65Fixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "pre_recovery.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name          string         `json:"name"`
			Tool          string         `json:"tool"`
			Failure       string         `json:"failure"`
			Output        map[string]any `json:"output"`
			Bypass        map[string]any `json:"bypass"`
			Diagnostic    bool           `json:"diagnostic"`
			PendingAbsent bool           `json:"pending_absent"`
			LeaseState    string         `json:"lease_state"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil || fixture.Oracle != "Python codex.pre_tool_use recovery" {
		t.Fatalf("fixture: %v %q", err, fixture.Oracle)
	}
	pluginRoot, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			origLease, origPending, origSession, origReady, origDecision, origBypass, origMetadata := afterPreLease, afterPrePending, afterPreSession, writeAsyncReady, runPreDecision, writePreBypass, afterPreMetadata
			t.Cleanup(func() {
				afterPreLease, afterPrePending, afterPreSession, writeAsyncReady, runPreDecision, writePreBypass, afterPreMetadata = origLease, origPending, origSession, origReady, origDecision, origBypass, origMetadata
			})
			afterPreLease, afterPrePending, afterPreSession = func() error { return nil }, func() error { return nil }, func() error { return nil }
			writeAsyncReady, runPreDecision = atomicAdmissionJSON, preDecision
			fail := errors.New("p65 injected failure")
			switch tc.Failure {
			case "after_lease":
				afterPreLease = func() error { return fail }
			case "after_pending":
				afterPrePending = func() error { return fail }
			case "after_session":
				afterPreSession = func() error { return fail }
			case "async_ready":
				writeAsyncReady = func(string, any) error { return fail }
			case "bypass_persist":
				afterPrePending = func() error { return fail }
				writePreBypass = func(string, map[string]any, string, map[string]any) error { return fail }
			case "panic":
				afterPreMetadata = func() { panic("p65 injected panic") }
			}
			scratch := preTempDir(t)
			repo, state := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			home := filepath.Join(scratch, "home")
			_ = os.MkdirAll(home, 0700)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("PURSER_STATE_DIR", state)
			t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			name := tc.Name
			prompt := "ORCH|task=" + name + "|phase=implementation|route=subtle|scope=src/a.go\nImplement the change."
			input := map[string]any{"prompt": prompt, "mode": "new", "timeoutSeconds": 90, "label": "P65 recovery"}
			if strings.HasSuffix(tc.Tool, "__codex_start") {
				input["mode"] = "new"
			}
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": tc.Tool, "tool_use_id": name, "session_id": "p65-session", "cwd": repo, "tool_input": input}
			got, _ := Pre(event, state)
			response := got["hookSpecificOutput"]
			if !p65JSONEqual(response, tc.Output) {
				t.Fatalf("output mismatch: got %#v want %#v", response, tc.Output)
			}
			pendingPath, _ := pendingPath(state, event)
			pendingRaw, readErr := os.ReadFile(pendingPath)
			pendingAbsent := os.IsNotExist(readErr)
			var bypass map[string]any
			if readErr == nil {
				_ = json.Unmarshal(pendingRaw, &bypass)
				pendingAbsent = bypass["kind"] == "pretool_bypass"
			}
			if pendingAbsent != tc.PendingAbsent {
				t.Fatalf("pending absent=%v want %v; record=%s", pendingAbsent, tc.PendingAbsent, pendingRaw)
			}
			if tc.Bypass == nil {
				if bypass != nil && bypass["kind"] == "pretool_bypass" {
					t.Fatalf("unexpected bypass: %#v", bypass)
				}
			} else {
				for k, want := range tc.Bypass {
					if want == "__PROJECT_KEY__" {
						want = projectKey(&repo)
					}
					if want == "__REPO__" {
						want = repo
					}
					if !p65JSONEqual(bypass[k], want) {
						t.Errorf("bypass.%s=%#v want %#v", k, bypass[k], want)
					}
				}
			}
			_, diagErr := os.Stat(filepath.Join(state, "diagnostics.jsonl"))
			if (!os.IsNotExist(diagErr)) != tc.Diagnostic {
				t.Fatalf("diagnostic presence mismatch: %v", diagErr)
			}
			leaseState := "NONE"
			project := projectKey(&repo)
			leasePath := filepath.Join(state, "projects", project, "scope_leases.json")
			if leaseRaw, e := os.ReadFile(leasePath); e == nil {
				var v map[string]any
				_ = json.Unmarshal(leaseRaw, &v)
				owners := mapValue(v["owners"])
				for _, owner := range owners {
					if state := stringValue(mapValue(owner)["state"]); state != "" {
						leaseState = state
					}
				}
			}
			if leaseState != tc.LeaseState {
				t.Fatalf("lease state=%q want %q", leaseState, tc.LeaseState)
			}
		})
	}
}

func p65JSONEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
