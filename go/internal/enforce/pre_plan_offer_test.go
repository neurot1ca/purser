package enforce

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestPrePlanOffer(t *testing.T) {
	for _, tc := range []struct {
		name, tool, phase, route, task, scope string
		priorPrompt                           bool
		want                                  bool
	}{
		{"large", "codex", "implementation", "normal", "large", "a,b,c,d", false, true},
		{"consequential", "codex_start", "implementation", "consequential", "risk", "a", false, true},
		{"escalated", "codex", "implementation", "escalated", "retry", "a", false, true},
		{"small", "codex", "implementation", "subtle", "small", "a,b,c", false, false},
		{"plan", "codex_start", "plan", "plan_standard", "planning", "a,b,c,d", false, false},
		{"reply", "codex-reply", "implementation", "subtle", "task-recorded", "a,b,c,d", false, false},
		{"async-reply", "codex_start", "implementation", "subtle", "task-recorded", "a,b,c,d", false, false},
		{"accepted-milestone", "codex", "implementation", "consequential", "planning-m1of2", "a,b,c,d", false, false},
		{"prompt-already-offered", "codex", "implementation", "normal", "large", "a,b,c,d", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := preTempDir(t)
			repo, state, home := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state"), filepath.Join(scratch, "home")
			for _, dir := range []string{repo, home} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			plugin, _ := filepath.Abs(filepath.Join("..", "..", ".."))
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("PURSER_STATE_DIR", state)
			t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			if tc.priorPrompt {
				contextio.UserPromptSubmit(map[string]any{"cwd": repo, "session_id": "offer-session", "prompt": "We need planning"}, time.Now())
			}
			reply := tc.tool == "codex-reply" || tc.name == "async-reply"
			if reply {
				seedPreThread(t, repo, state)
			}
			for call := 0; call < 2; call++ {
				task := tc.task
				if call > 0 {
					task += "-second"
				}
				scope := tc.scope
				if call > 0 {
					scope = "second/" + strings.ReplaceAll(scope, ",", ",second/")
				}
				prompt := fmt.Sprintf("ORCH|task=%s|phase=%s|route=%s|reason=new_task|basis=choose the design|scope=%s\nImplement.", task, tc.phase, tc.route, scope)
				input := map[string]any{"prompt": prompt, "label": "offer test"}
				if reply {
					input["threadId"] = "thread-p63"
					input["mode"] = "reply"
				}
				event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__codex-main__" + tc.tool, "tool_use_id": fmt.Sprint(call), "session_id": "offer-session", "cwd": repo, "tool_input": input}
				got, err := Pre(event, state)
				hook := mapValue(got["hookSpecificOutput"])
				if err != nil || hook["permissionDecision"] != nil || hook["updatedInput"] == nil {
					t.Fatalf("call not allowed: %#v %v", got, err)
				}
				line := stringValue(hook["additionalContext"])
				want := tc.want && call == 0
				if strings.Contains(line, "offer /purser:plan") != want {
					t.Fatalf("offer=%q want %v", line, want)
				}
				if want && line != "This task looks large enough for a plan; offer /purser:plan once in this session if you have not already." {
					t.Fatalf("unexpected offer: %q", line)
				}
				if strings.Contains(stringValue(mapValue(hook["updatedInput"])["prompt"]), "offer /purser:plan") {
					t.Fatal("offer grew the worker prompt")
				}
				if reply || tc.name == "accepted-milestone" {
					break
				}
			}
			if tc.want {
				output := contextio.UserPromptSubmit(map[string]any{"cwd": repo, "session_id": "offer-session", "prompt": "We need planning"}, time.Now())
				if strings.Contains(string(output), "offer /purser:plan") {
					t.Fatal("prompt repeated the pre-hook offer")
				}
			}
		})
	}
}

func TestPlanOfferConcurrentAndUnavailableState(t *testing.T) {
	root := preTempDir(t)
	var workers sync.WaitGroup
	offers := make(chan string, 8)
	for i := 0; i < cap(offers); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			offers <- contextio.ImplementationPlanOffer(root, "project", "session", "task", "consequential", nil)
		}()
	}
	workers.Wait()
	close(offers)
	count := 0
	for offer := range offers {
		if offer != "" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent offers=%d", count)
	}
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if offer := contextio.ImplementationPlanOffer(blocked, "project", "session", "task", "consequential", nil); offer != "" {
		t.Fatalf("unpersisted offer: %q", offer)
	}
}
