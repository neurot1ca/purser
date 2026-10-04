package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestPreFallbackGapsP63BFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "pre_fallback_gaps.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle   string         `json:"oracle"`
		Fallback map[string]any `json:"fallback"`
		Cases    []struct {
			Name   string `json:"name"`
			Launch []struct {
				ID       string  `json:"id"`
				State    string  `json:"state"`
				Finished float64 `json:"finishedAt"`
				Type     string  `json:"type"`
			} `json:"launch"`
			Active       string         `json:"active"`
			Quota        string         `json:"quota"`
			Outcome      string         `json:"outcome"`
			TriggerID    string         `json:"trigger_id"`
			Question     string         `json:"question"`
			Record       map[string]any `json:"record"`
			Observations []string       `json:"observations"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil || fixture.Oracle != "Python codex.pre_tool_use" {
		t.Fatalf("fixture decode: %v %q", err, fixture.Oracle)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			scratch := preTempDir(t)
			repo, runtimeRoot := filepath.Join(scratch, "repo"), filepath.Join(scratch, "state")
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("Claude runs the full suite once before acceptance.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(scratch, "home")
			if err := os.MkdirAll(filepath.Join(home, ".orchestrator"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".orchestrator", "config.toml"), []byte("[fallback]\nagent = \"opencode\"\nmodel = \"anthropic/claude-sonnet\"\nfamily = \"anthropic\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOMEDRIVE", "")
			t.Setenv("HOMEPATH", "")
			t.Setenv("PURSER_STATE_DIR", runtimeRoot)
			t.Setenv("CLAUDE_PLUGIN_ROOT", mustPluginRoot(t))
			t.Setenv("CODEX_HOME", filepath.Join(scratch, "codex"))
			project := projectKey(&repo)
			if item.Active != "malformed" {
				active := map[string]any{"active_coder": "primary", "triggering_evidence": nil, "reset_cycle_marker": nil, "last_probe_time": nil}
				switch item.Active {
				case "primary_asked":
					active["triggering_evidence"] = map[string]any{"type": "CREDIT_EXHAUSTED", "trigger_id": "asked", "question": "already asked", "question_session": "session-p63b", "question_shown": false, "answerable": false}
				case "fallback_quota":
					active["active_coder"] = "fallback"
					marker := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
					reset := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
					evidence := map[string]any{"type": "QUOTA", "trigger_id": "old-cycle", "reset_cycle_marker": marker}
					if item.Quota == "same_cycle" {
						evidence["declined_reset_cycle_marker"] = reset
					}
					active["triggering_evidence"], active["reset_cycle_marker"] = evidence, marker
					writePreQuota(t, filepath.Join(scratch, "codex"), reset)
				}
				activePath := filepath.Join(runtimeRoot, "projects", project, "active-coder.json")
				if err := os.MkdirAll(filepath.Dir(activePath), 0700); err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(active)
				if err := os.WriteFile(activePath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				activePath := filepath.Join(runtimeRoot, "projects", project, "active-coder.json")
				if err := os.MkdirAll(filepath.Dir(activePath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(activePath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, launch := range item.Launch {
				for _, dir := range []string{filepath.Join(runtimeRoot, "async_jobs"), filepath.Join(runtimeRoot, "codex_exec_mcp", "jobs")} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				index, _ := json.Marshal(map[string]any{"jobId": launch.ID, "project_key": project})
				job := map[string]any{"jobId": launch.ID, "phase": "implementation", "state": launch.State, "finishedAt": launch.Finished}
				if launch.Type != "" {
					job["launchFailureEvidence"] = map[string]any{"type": launch.Type}
				}
				jobData, _ := json.Marshal(job)
				if err := os.WriteFile(filepath.Join(runtimeRoot, "async_jobs", launch.ID+".json"), index, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(runtimeRoot, "codex_exec_mcp", "jobs", launch.ID+".json"), jobData, 0600); err != nil {
					t.Fatal(err)
				}
			}
			prompt := "ORCH|task=task-1|phase=implementation|route=normal|reason=new_task|basis=specific repository boundary|scope=AGENTS.md\nDo the work."
			event := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "mcp__plugin_purser_codex-main__codex", "tool_input": map[string]any{"prompt": prompt, "label": "fixture", "cwd": repo}, "session_id": "session-1", "tool_use_id": "use-p63b"}
			got, gotErr := Pre(event, runtimeRoot)
			out := mapValue(got["hookSpecificOutput"])
			switch item.Outcome {
			case "internal_failure":
				if gotErr == nil || out["additionalContext"] == nil {
					t.Fatalf("malformed state did not fail open: %#v %v", got, gotErr)
				}
			case "allow":
				if gotErr != nil || mapValue(out["updatedInput"]) == nil {
					t.Fatalf("expected allowed input rewrite, got %#v %v", got, gotErr)
				}
			case "fallback_question", "deny_asked":
				if gotErr != nil || out["permissionDecision"] != "deny" || !strings.Contains(stringValue(out["permissionDecisionReason"]), "FALLBACK_CONSENT_REQUIRED") {
					t.Fatalf("fallback denial mismatch: %#v %v", got, gotErr)
				}
				active, err := contextio.ReadProjectActiveCoder(repo, runtimeRoot)
				if err != nil {
					t.Fatal(err)
				}
				evidence := mapValue(active["triggering_evidence"])
				if item.Outcome == "fallback_question" {
					if !reflect.DeepEqual(evidence, item.Record) {
						t.Fatalf("fallback record: %#v", evidence)
					}
				} else if evidence["question"] != "already asked" {
					t.Fatalf("existing question changed: %#v", evidence)
				}
				advisorPath := filepath.Join(runtimeRoot, "projects", project, "telemetry", "compaction_advisor.json")
				advisor, err := os.ReadFile(advisorPath)
				var advisorState map[string]any
				if err == nil {
					err = json.Unmarshal(advisor, &advisorState)
				}
				pending := mapValue(advisorState["fallback_pending"])
				if item.Outcome == "fallback_question" && (err != nil || pending["line"] != item.Question || pending["model"] != "anthropic/claude-sonnet" || pending["project"] != repo || pending["notice"] != false || pending["blocked"] != false || pending["delivered"] != false) {
					t.Fatalf("queued question missing: %v %#v", err, pending)
				}
				if item.Outcome == "deny_asked" && !os.IsNotExist(err) {
					t.Fatalf("already-asked question was requeued: %s", advisor)
				}
			case "return_offer":
				if gotErr != nil || out["permissionDecision"] != "deny" || !strings.Contains(stringValue(out["permissionDecisionReason"]), "PRIMARY_RETURN_CONSENT_REQUIRED") {
					t.Fatalf("return offer mismatch: %#v %v", got, gotErr)
				}
				active, err := contextio.ReadProjectActiveCoder(repo, runtimeRoot)
				if err != nil {
					t.Fatal(err)
				}
				evidence := mapValue(active["triggering_evidence"])
				if evidence["question"] != item.Question || evidence["question_session"] != "session-1" || evidence["question_shown"] != false || evidence["answerable"] != false {
					t.Fatalf("return offer not recorded: %#v", evidence)
				}
				if _, ok := evidence["question_reset_marker"].(string); !ok {
					t.Fatalf("return reset marker missing: %#v", evidence)
				}
				advisor, err := os.ReadFile(filepath.Join(runtimeRoot, "projects", project, "telemetry", "compaction_advisor.json"))
				var advisorState map[string]any
				if err == nil {
					err = json.Unmarshal(advisor, &advisorState)
				}
				pending := mapValue(advisorState["fallback_pending"])
				if err != nil || pending["line"] != item.Question || pending["model"] != "gpt-6-sol" || pending["project"] != repo || pending["notice"] != false || pending["blocked"] != false || pending["delivered"] != false {
					t.Fatalf("return question not queued: %v %#v", err, pending)
				}
			}
			if gotObservations := preFallbackObservationTypes(t, filepath.Join(runtimeRoot, "telemetry", "events")); !equalStrings(gotObservations, item.Observations) {
				t.Fatalf("observation events = %v, want %v", gotObservations, item.Observations)
			}
		})
	}
}

func preFallbackObservationTypes(t *testing.T, root string) []string {
	t.Helper()
	var result []string
	months, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return result
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, month := range months {
		if !month.IsDir() || len(month.Name()) != 7 {
			continue
		}
		path := filepath.Join(root, month.Name(), "session_session-1.jsonl")
		data, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var event map[string]any
			if json.Unmarshal(line, &event) == nil {
				if kind, ok := event["event_type"].(string); ok {
					result = append(result, kind)
				}
			}
		}
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func mustPluginRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writePreQuota(t *testing.T, home, reset string) {
	t.Helper()
	now := time.Now().UTC()
	resetAt, err := time.Parse(time.RFC3339, reset)
	if err != nil {
		t.Fatal(err)
	}
	day := now.Format("2006/01/02")
	dir := filepath.Join(home, "sessions", filepath.FromSlash(day))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"timestamp": now.Format(time.RFC3339), "payload": map[string]any{"rate_limits": map[string]any{"primary": map[string]any{"window_minutes": 300, "used_percent": 96, "resets_at": float64(resetAt.UnixNano()) / 1e9}}}}
	data, _ := json.Marshal(event)
	if err := os.WriteFile(filepath.Join(dir, "rollout-p63b.jsonl"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
