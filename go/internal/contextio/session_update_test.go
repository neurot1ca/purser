package contextio

import "purser-hook/internal/testutil"

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func updateSessionTestRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := testutil.Hide(exec.Command("git", "init", "--quiet", repo)).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return repo
}

func sessionStartContext(t *testing.T, event map[string]any) string {
	t.Helper()
	var output map[string]any
	if err := json.Unmarshal(SessionStart(event), &output); err != nil {
		t.Fatalf("SessionStart output: %v", err)
	}
	hook, _ := output["hookSpecificOutput"].(map[string]any)
	context, _ := hook["additionalContext"].(string)
	return context
}

func TestSessionStartPluginVersionNoticeParity(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	repo := updateSessionTestRepo(t)
	plugin := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	manifest := filepath.Join(plugin, ".claude-plugin", "plugin.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(manifest, `{"version":"2.0.0"}`)
	changelog := filepath.Join(plugin, "CHANGELOG.md")
	write(changelog, "## 2.0.0 — 2026-09-28\n\n- First change.\n- Second change.\n## 1.9.0 — 2026-09-20\n- Old change.\n")
	versionPath := filepath.Join(stateRoot, "plugin-version.json")
	statePath := filepath.Join(stateRoot, "projects", repositoryProjectKey(repo), "telemetry", "compaction_advisor.json")
	event := map[string]any{"hook_event_name": "SessionStart", "session_id": "session-update", "source": "startup", "cwd": repo}

	first := sessionStartContext(t, event)
	if strings.Contains(first, "Purser upgraded") {
		t.Fatalf("first install unexpectedly announced version: %s", first)
	}
	var recorded map[string]any
	data, err := os.ReadFile(versionPath)
	if err != nil || json.Unmarshal(data, &recorded) != nil || recorded["announced_version"] != "2.0.0" {
		t.Fatalf("first install version state = %s, err=%v", data, err)
	}
	state := readAdvisorState(statePath, "session-update")
	if state["version_pending"] != nil {
		t.Fatalf("first install queued version notice: %#v", state["version_pending"])
	}
	if repeated := sessionStartContext(t, event); strings.Contains(repeated, "Purser upgraded") {
		t.Fatalf("unchanged version unexpectedly announced: %s", repeated)
	}

	write(versionPath, `{"announced_version":"1.8.0"}`)
	changed := sessionStartContext(t, event)
	want := "On your next reply, put exactly one notice line first, then handle the user's request in full; the notice never replaces the task. Render the notice in the user's language, translating from English, in this form: `> ⚓ **Purser upgraded to 2.0.0:** <one short sentence summarizing the bullets>`. Keep ⚓ and 2.0.0. Source bullets (summarize them in one short sentence):\n- First change.\n- Second change."
	if !strings.Contains(changed, want) || strings.Contains(changed, "Old change.") {
		t.Fatalf("version notice differs from Python notice: %q", changed)
	}
	if repeated := sessionStartContext(t, event); strings.Contains(repeated, "Purser upgraded") {
		t.Fatalf("delivered version notice repeated: %s", repeated)
	}
	state = readAdvisorState(statePath, "session-update")
	pending, ok := state["version_pending"].(map[string]any)
	if !ok || pending["version"] != "2.0.0" {
		t.Fatalf("version pending state = %#v", state["version_pending"])
	}
	newSession := map[string]any{"hook_event_name": "SessionStart", "session_id": "session-update-new", "source": "startup", "cwd": repo}
	if repeated := sessionStartContext(t, newSession); strings.Contains(repeated, "Purser upgraded") {
		t.Fatalf("delivered version notice repeated in a new session: %s", repeated)
	}
}

func TestSessionStartReportsModelChangeOnce(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	repo := updateSessionTestRepo(t)
	path := filepath.Join(stateRoot, "model-version-notice.json")
	if err := os.WriteFile(path, []byte(`{"report":{"current":"gpt-6-luna","new":"gpt-6.1-luna","delivered":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"hook_event_name": "SessionStart", "session_id": "model-report", "source": "startup", "cwd": repo}
	want := "> ⚓ **Purser now uses `gpt-6.1-luna` (was `gpt-6-luna`).**"
	first := sessionStartContext(t, event)
	if !strings.Contains(first, want) || strings.Contains(strings.ToLower(first), "adopt") || strings.Contains(strings.ToLower(first), "decline") || strings.Contains(strings.ToLower(first), "yes") {
		t.Fatalf("model report output = %q", first)
	}
	if repeated := sessionStartContext(t, event); strings.Contains(repeated, want) {
		t.Fatalf("model report repeated: %q", repeated)
	}
}

func TestSessionStartMechanicalLeaderNoticeParity(t *testing.T) {
	stateRoot := t.TempDir()
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	t.Setenv("CLAUDE_PLUGIN_ROOT", t.TempDir())
	repo := updateSessionTestRepo(t)
	leaderPath := filepath.Join(stateRoot, "opencode", "leader.json")
	if err := os.MkdirAll(filepath.Dir(leaderPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leaderPath, []byte(`{"model":"vendor/free-model","successes":7,"sample_count":9,"announced":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	inventoryPath := filepath.Join(stateRoot, "opencode", "candidates.json")
	if err := os.WriteFile(inventoryPath, []byte(`{"candidates":[{"id":"vendor/free-model","status":"measured","mechanical_samples":[true,true,true,true,true,true,true,false,false]},{"id":"vendor/old","status":"measured","mechanical_samples":[true,true,true,true,false,false,false,false,false]}],"race":{"started":true,"finished":true,"winner":"vendor/free-model","finished_at":"2026-10-16T00:00:00Z","stage":"newcomer_vs_leader","entrants":["vendor/free-model","vendor/old"],"samples":{"vendor/free-model":[true,true,true]},"notice_claim_ids":[]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"hook_event_name": "SessionStart", "session_id": "session-leader", "source": "startup", "cwd": repo}
	context := sessionStartContext(t, event)
	want := "On your next reply, render exactly one line in the user's language, translating from the English text and keeping ⚓, markdown, model names, numbers, and commands unchanged, in this form: " +
		"> ⚓ **Free model ranking changed:** `vendor/free-model` succeeded in 7 of its last 9 attempts and moved to the top of the ranking. Mechanical tasks will go to this model first."
	if !strings.Contains(context, want) {
		t.Fatalf("leader notice differs from Python notice: %q", context)
	}
	statePath := filepath.Join(stateRoot, "projects", repositoryProjectKey(repo), "telemetry", "compaction_advisor.json")
	state := readAdvisorState(statePath, "session-leader")
	pending, ok := state["leader_pending"].(map[string]any)
	if !ok || pending["model"] != "vendor/free-model" || pending["successes"] != float64(7) || pending["sample_count"] != float64(9) {
		t.Fatalf("leader pending state = %#v", state["leader_pending"])
	}
}
