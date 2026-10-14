package enforce

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"strings"
	"testing"
)

func TestPrePlanRedirect(t *testing.T) {
	tests := []struct {
		name, task, phase, route, reason string
		scope                            []string
		reply, completedPlan, wantDeny   bool
	}{
		{name: "six non-test paths allow", task: "six-paths", phase: "implementation", route: "normal", reason: "new_task", scope: []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}},
		{name: "four paths plus tests allow", task: "four-plus-tests", phase: "implementation", route: "normal", reason: "new_task", scope: []string{"a.go", "b.go", "c.go", "d.go", "tests/test_a.py", "test_unit.py", "x_test.go"}},
		{name: "consequential one path deny", task: "consequential", phase: "implementation", route: "consequential", reason: "new_task", scope: []string{"a.go"}, wantDeny: true},
		{name: "mechanical ten paths allow", task: "mechanical", phase: "implementation", route: "mechanical", reason: "new_task", scope: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}},
		{name: "operation route allows", task: "operation-route", phase: "implementation", route: "operation", reason: "new_task", scope: []string{"a", "b", "c", "d", "e"}},
		{name: "fix allows", task: "fix-task", phase: "implementation", route: "normal", reason: "fix", scope: []string{"a", "b", "c", "d", "e"}},
		{name: "continue allows", task: "continue-task", phase: "implementation", route: "normal", reason: "continue", scope: []string{"a", "b", "c", "d", "e"}},
		{name: "route change allows", task: "route-change-task", phase: "implementation", route: "normal", reason: "route_change", scope: []string{"a", "b", "c", "d", "e"}},
		{name: "reanchor allows", task: "reanchor-task", phase: "implementation", route: "normal", reason: "reanchor", scope: []string{"a", "b", "c", "d", "e"}},
		{name: "user override allows", task: "override-task", phase: "implementation", route: "normal", reason: "user_override", scope: []string{"a", "b", "c", "d", "e"}},
		{name: "reply allows", task: "task-recorded", phase: "implementation", route: "subtle", reason: "new_task", scope: []string{"a", "b", "c", "d", "e"}, reply: true},
		{name: "planned milestone naming exempt", task: "foo-m2of3", phase: "implementation", route: "normal", reason: "new_task", scope: []string{"a", "b", "c", "d", "e"}, completedPlan: true},
		{name: "planned arbitrary child exempt", task: "foo-anything", phase: "implementation", route: "normal", reason: "new_task", scope: []string{"a", "b", "c", "d", "e"}, completedPlan: true},
		{name: "milestone name without plan allows", task: "bar-m2of3", phase: "implementation", route: "normal", reason: "new_task", scope: []string{"a", "b", "c", "d", "e"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scratch := preTempDir(t)
			repo, state, home := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state"), filepath.Join(scratch, "home")
			for _, dir := range []string{repo, home} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			plugin, _ := filepath.Abs(filepath.Join("..", "..", ".."))
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("PURSER_STATE_DIR", state)
			t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			if tc.completedPlan {
				if err := os.MkdirAll(filepath.Join(state, "plans"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "plans", "foo-"+strings.Repeat("a", 64)+".md"), []byte("plan"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tool := "mcp__codex-main__codex"
			input := map[string]any{}
			if tc.reply {
				seedPreThread(t, repo, state)
				tool = "mcp__codex-main__codex-reply"
				input["threadId"], input["mode"] = "thread-p63", "reply"
			}
			reason := ""
			if tc.reason != "" {
				reason = "|reason=" + tc.reason
			}
			basis := "bounded implementation"
			if tc.route == "consequential" {
				basis = "design:bounded implementation"
			}
			prompt := fmt.Sprintf("ORCH|task=%s|phase=%s|route=%s%s|basis=%s|scope=%s\nImplement.", tc.task, tc.phase, tc.route, reason, basis, strings.Join(tc.scope, ","))
			input["prompt"] = prompt
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_use_id": "plan-redirect-call", "session_id": "plan-redirect-session", "cwd": repo, "tool_input": input}
			got, err := Pre(event, state)
			if err != nil {
				t.Fatalf("Pre: %v", err)
			}
			hook := mapValue(got["hookSpecificOutput"])
			denied := hook["permissionDecision"] == "deny"
			if denied != tc.wantDeny {
				t.Fatalf("denied=%v want %v; output=%#v", denied, tc.wantDeny, got)
			}
			if tc.wantDeny {
				reason := stringValue(hook["permissionDecisionReason"])
				why := "route=" + tc.route
				wantReason := fmt.Sprintf("Purser: this is a large new task (%s). Run /purser:plan with the same goal and scope first, then implement its milestones as <plan-task>-m<N>of<T>. If the user explicitly asked to skip planning, resend with reason=user_override.", why)
				if reason != wantReason {
					t.Fatalf("redirect reason=%q", reason)
				}
				if matches, _ := filepath.Glob(filepath.Join(state, "pending", "*.json")); len(matches) != 0 {
					t.Fatalf("pending files created: %v", matches)
				}
				if matches, _ := filepath.Glob(filepath.Join(state, "projects", "*", "scope_leases.json")); len(matches) != 0 {
					t.Fatalf("lease files created: %v", matches)
				}
				telemetry, err := os.ReadFile(filepath.Join(state, "telemetry", "events", "by_task", tc.task+".jsonl"))
				if err != nil {
					t.Fatalf("PLAN_REDIRECT telemetry missing: %v", err)
				}
				if lines := strings.Count(string(telemetry), "\n"); lines != 1 {
					t.Fatalf("PLAN_REDIRECT event count=%d want 1", lines)
				}
				var event map[string]any
				if err := json.Unmarshal(telemetry, &event); err != nil {
					t.Fatalf("invalid telemetry: %v", err)
				}
				payload := mapValue(event["payload"])
				if event["event_type"] != "PLAN_REDIRECT" || event["task_id"] != tc.task || payload["route"] != tc.route || payload["non_test_scope_count"] != float64(nonTestScopeCount(tc.scope)) {
					t.Fatalf("unexpected redirect telemetry: %#v", event)
				}
			} else if tc.task == "six-paths" {
				telemetry, err := os.ReadFile(filepath.Join(state, "telemetry", "events", "by_task", tc.task+".jsonl"))
				if err == nil {
					for _, line := range strings.Split(strings.TrimSpace(string(telemetry)), "\n") {
						var event map[string]any
						if json.Unmarshal([]byte(line), &event) == nil && event["event_type"] == "PLAN_REDIRECT" {
							t.Fatalf("unexpected PLAN_REDIRECT telemetry: %s", line)
						}
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("read telemetry: %v", err)
				}
			}
		})
	}
}
