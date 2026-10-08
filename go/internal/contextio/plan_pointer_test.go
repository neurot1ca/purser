package contextio

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func planPointerFixture(t *testing.T, milestones []int) (string, string, string, *PlanState) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	stateRoot := filepath.Join(root, "state")
	artifact := filepath.Join(root, "accepted-plan.md")
	if err := os.WriteFile(artifact, []byte("accepted plan"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	state, err := AcceptPlan(stateRoot, repo, "plan-pointer", artifact, milestones)
	if err != nil {
		t.Fatal(err)
	}
	return stateRoot, repo, artifact, state
}

func planPointerContext(t *testing.T, output []byte) string {
	t.Helper()
	if len(output) == 0 {
		return ""
	}
	var response map[string]any
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("hook output %q: %v", output, err)
	}
	hook, _ := response["hookSpecificOutput"].(map[string]any)
	context, _ := hook["additionalContext"].(string)
	return context
}

func TestPlanPointerRestoresWithoutFreshHandoff(t *testing.T) {
	for _, handoff := range []string{"missing", "stale"} {
		t.Run(handoff, func(t *testing.T) {
			stateRoot, repo, _, state := planPointerFixture(t, []int{2, 4, 5})
			if handoff == "stale" {
				path := filepath.Join(stateRoot, "projects", repositoryProjectKey(repo), "telemetry", "purser-handoff.md")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("# Old handoff\nPlan plan-pointer: milestone 3 of 3; next action: obsolete.\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			event := map[string]any{"hook_event_name": "SessionStart", "session_id": "pointer-session-" + handoff, "source": "startup", "cwd": repo}
			got := planPointerContext(t, SessionStart(event))
			want := "Plan plan-pointer: milestone 2 of 3; 0 applied passes; next action: dispatch; plan: " + state.ArtifactPath + "."
			if !strings.Contains(got, want) {
				t.Fatalf("SessionStart context missing pointer %q: %s", want, got)
			}
		})
	}
}

func TestPlanPointerDeliveredAtCompactAndClear(t *testing.T) {
	_, repo, _, _ := planPointerFixture(t, []int{1, 2})
	for _, source := range []string{"compact", "clear"} {
		event := map[string]any{"hook_event_name": "SessionStart", "session_id": "pointer-" + source, "source": source, "cwd": repo}
		if got := planPointerContext(t, SessionStart(event)); !strings.Contains(got, "Plan plan-pointer:") {
			t.Errorf("%s SessionStart did not deliver pointer: %s", source, got)
		}
	}
}

func TestPlanPointerPromptDeduplicatesUnchangedAction(t *testing.T) {
	_, repo, _, _ := planPointerFixture(t, []int{1, 2})
	event := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "pointer-prompt", "cwd": repo, "prompt": "continue"}
	first := planPointerContext(t, UserPromptSubmit(event, time.Now()))
	second := planPointerContext(t, UserPromptSubmit(event, time.Now()))
	if !strings.Contains(first, "Plan plan-pointer:") || strings.Contains(second, "Plan plan-pointer:") {
		t.Fatalf("pointer prompt deliveries = first %q, second %q", first, second)
	}
}

func TestPlanPointerPromptReinjectsChangedAction(t *testing.T) {
	root, repo, _, state := planPointerFixture(t, []int{1, 2})
	event := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "pointer-changed", "cwd": repo, "prompt": "continue"}
	_ = UserPromptSubmit(event, time.Now())
	updated, err := UpdatePlan(root, repo, state.TaskID, state.Revision, "gate", "review")
	if err != nil {
		t.Fatal(err)
	}
	if got := planPointerContext(t, UserPromptSubmit(event, time.Now())); !strings.Contains(got, "Plan plan-pointer:") || !strings.Contains(got, "next action: gate: review") {
		t.Fatalf("changed action pointer = %q", got)
	}
	_ = updated
}

func TestPlanPointerAbsentStoppedOrCompleted(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		root := t.TempDir()
		repo := updateSessionTestRepo(t)
		t.Setenv("PURSER_STATE_DIR", root)
		if got := renderActivePlanPointer(repo); got != "" {
			t.Fatalf("absent plan pointer = %q", got)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		root, repo, _, state := planPointerFixture(t, []int{1})
		if _, err := UpdatePlan(root, repo, state.TaskID, state.Revision, "stop", ""); err != nil {
			t.Fatal(err)
		}
		if got := renderActivePlanPointer(repo); got != "" {
			t.Fatalf("stopped plan pointer = %q", got)
		}
	})
	t.Run("completed", func(t *testing.T) {
		root, repo, _, state := planPointerFixture(t, []int{1})
		if _, err := UpdatePlan(root, repo, state.TaskID, state.Revision, "complete-milestone", ""); err != nil {
			t.Fatal(err)
		}
		if got := renderActivePlanPointer(repo); got != "" {
			t.Fatalf("completed plan pointer = %q", got)
		}
	})
}

func TestPlanPointerReplacesJobScan(t *testing.T) {
	source, err := os.ReadFile("precompact.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "activePlanLine") || strings.Contains(string(source), "filepath.Glob(filepath.Join(stateRoot(), \"codex_exec_mcp\", \"jobs\"") {
		t.Fatal("legacy job-scan plan inference remains in precompact.go")
	}
}
