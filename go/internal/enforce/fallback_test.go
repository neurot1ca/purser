package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/policy"
)

func TestFallbackPreP36Fixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "fallback.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string         `json:"name"`
			Phase    string         `json:"phase"`
			Reason   string         `json:"reason"`
			Session  string         `json:"session"`
			Active   map[string]any `json:"active"`
			Fallback map[string]any `json:"fallback"`
			Quota    map[string]any `json:"quota"`
			Launch   map[string]any `json:"launch"`
			Now      string         `json:"now"`
			Expected map[string]any `json:"expected"`
		} `json:"fallback_pre_cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			if !strings.Contains(item.Name, "p36") {
				t.Fatalf("case name lacks p36: %s", item.Name)
			}
			stamp, err := time.Parse(time.RFC3339, item.Now)
			if err != nil {
				t.Fatal(err)
			}
			parsed := policy.OrchParseResult{Valid: true, Phase: &item.Phase, Reason: &item.Reason}
			got := decideFallbackPre(parsed, item.Session, item.Active, item.Fallback, item.Quota, item.Launch, stamp)
			for key, want := range item.Expected {
				var actual any
				switch key {
				case "action":
					actual = got.Action
				case "code":
					actual = got.Code
				case "message":
					actual = got.Message
				case "kind":
					actual = got.Kind
				case "question":
					actual = got.Question
				default:
					t.Fatalf("unknown expected field %q", key)
				}
				if actual != want {
					t.Errorf("%s = %#v, want %#v", key, actual, want)
				}
			}
			switch got.Action {
			case "approve", "decline":
				project := filepath.Join(t.TempDir(), item.Name)
				runtimeRoot := t.TempDir()
				writeFallbackFixtureState(t, project, runtimeRoot, item.Active)
				approved := got.Action == "approve"
				changed, err := contextio.ApplyFallbackConsent(project, runtimeRoot, item.Session, approved, nil, "now")
				if err != nil || !changed {
					t.Fatalf("answer transition = %v, %v", changed, err)
				}
				state := readFallbackFixtureState(t, project, runtimeRoot)
				if approved && state["active_coder"] != "fallback" {
					t.Fatalf("approval state = %#v", state)
				}
				if !approved {
					evidence := state["triggering_evidence"].(map[string]any)
					if evidence["declined"] != true || evidence["trigger_id"] != "job-2" {
						t.Fatalf("declined evidence = %#v", evidence)
					}
				}
			case "deny":
				if item.Name == "p36-approval-races-new-question" {
					project := filepath.Join(t.TempDir(), item.Name)
					runtimeRoot := t.TempDir()
					writeFallbackFixtureState(t, project, runtimeRoot, item.Active)
					if changed, err := contextio.ApplyFallbackConsent(project, runtimeRoot, item.Session, true, nil, "now"); err != nil || changed {
						t.Fatalf("stale approval = %v, %v", changed, err)
					}
				}
			}
		})
	}
}

func TestFallbackReturnP37Fixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "fallback.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name          string         `json:"name"`
			Session       string         `json:"session"`
			Reason        string         `json:"reason"`
			Now           string         `json:"now"`
			Active        map[string]any `json:"active"`
			Quota         map[string]any `json:"quota"`
			UpdateTrigger map[string]any `json:"update_trigger"`
			Expected      string         `json:"expected"`
		} `json:"fallback_return_cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Cases {
		t.Run(item.Name, func(t *testing.T) {
			if !strings.Contains(item.Name, "p37") {
				t.Fatalf("case name lacks p37: %s", item.Name)
			}
			now, err := time.Parse(time.RFC3339, item.Now)
			if err != nil {
				t.Fatal(err)
			}
			if item.UpdateTrigger == nil {
				if got := decideFallbackReturn(item.Active, item.Session, item.Reason, item.Quota, now); got != item.Expected {
					t.Fatalf("decision = %q, want %q", got, item.Expected)
				}
			}
			if item.Expected == "accept" || item.Expected == "decline" {
				project, root := filepath.Join(t.TempDir(), item.Name), t.TempDir()
				writeFallbackFixtureState(t, project, root, item.Active)
				evidence := item.Active["triggering_evidence"].(map[string]any)
				changed, err := contextio.ApplyPrimaryReturnConsent(project, root, item.Session,
					evidence["type"].(string), evidence["trigger_id"], item.Expected == "accept")
				if err != nil || !changed {
					t.Fatalf("return consent transition = %v, %v", changed, err)
				}
				state := readFallbackFixtureState(t, project, root)
				if item.Expected == "accept" && state["active_coder"] != "primary" {
					t.Fatalf("accepted return state = %#v", state)
				}
				if item.Expected == "decline" && state["active_coder"] != "fallback" {
					t.Fatalf("declined return state = %#v", state)
				}
			}
			if item.Expected == "offer" {
				project, root := filepath.Join(t.TempDir(), item.Name), t.TempDir()
				writeFallbackFixtureState(t, project, root, item.Active)
				evidence := item.Active["triggering_evidence"].(map[string]any)
				changed, err := contextio.RecordPrimaryReturnQuestion(project, root, "QUOTA", evidence["trigger_id"],
					item.Session, "quota renewed", item.Quota["5h_resets_at"])
				if err != nil || !changed {
					t.Fatalf("return question record = %v, %v", changed, err)
				}
				state := readFallbackFixtureState(t, project, root)
				if state["triggering_evidence"].(map[string]any)["question_session"] != item.Session {
					t.Fatalf("return question state = %#v", state)
				}
			}
			if item.Expected == "deny" {
				project, root := filepath.Join(t.TempDir(), item.Name), t.TempDir()
				writeFallbackFixtureState(t, project, root, item.Active)
				evidence := item.Active["triggering_evidence"].(map[string]any)
				changed, err := contextio.ApplyPrimaryReturnConsent(project, root, item.Session,
					evidence["type"].(string), evidence["trigger_id"], true)
				if err != nil || changed {
					t.Fatalf("stale return consent = %v, %v", changed, err)
				}
				if state := readFallbackFixtureState(t, project, root); state["active_coder"] != "fallback" {
					t.Fatalf("stale consent changed active coder: %#v", state)
				}
			}
			if item.Expected == "claim_probe" {
				project, root := filepath.Join(t.TempDir(), item.Name), t.TempDir()
				evidence := item.Active["triggering_evidence"].(map[string]any)
				writeFallbackFixtureState(t, project, root, item.Active)
				id := evidence["trigger_id"]
				stamp := float64(now.UnixNano()) / 1e9
				claimed, err := contextio.ClaimFallbackProbe(project, root, evidence["type"].(string), id, stamp)
				if err != nil || !claimed {
					t.Fatalf("first probe claim = %v, %v", claimed, err)
				}
				claimed, err = contextio.ClaimFallbackProbe(project, root, evidence["type"].(string), id, stamp)
				if err != nil || claimed {
					t.Fatalf("second probe claim = %v, %v", claimed, err)
				}
			}
			if item.Expected == "cooldown_return" || item.Name == "p37-rate-limit-cooldown-wait" {
				project, root := filepath.Join(t.TempDir(), item.Name), t.TempDir()
				writeFallbackFixtureState(t, project, root, item.Active)
				evidence := item.Active["triggering_evidence"].(map[string]any)
				changed, err := contextio.ApplyPrimaryReturnCooldown(project, root, evidence["trigger_id"], float64(now.UnixNano())/1e9)
				want := item.Expected == "cooldown_return"
				if err != nil || changed != want {
					t.Fatalf("cooldown transition = %v, %v; want %v", changed, err, want)
				}
			}
			if item.Expected == "guarded" {
				project, root := filepath.Join(t.TempDir(), item.Name), t.TempDir()
				writeFallbackFixtureState(t, project, root, item.Active)
				changed, err := contextio.UpdateFallbackTrigger(project, root,
					item.UpdateTrigger["type"].(string), item.UpdateTrigger["trigger_id"],
					func(state map[string]any) { state["active_coder"] = "primary" })
				if err != nil || changed {
					t.Fatalf("stale trigger update = %v, %v", changed, err)
				}
				state := readFallbackFixtureState(t, project, root)
				if state["active_coder"] != "fallback" || state["triggering_evidence"].(map[string]any)["trigger_id"] != "new-credit" {
					t.Fatalf("stale trigger overwrote current state: %#v", state)
				}
			}
		})
	}
}

func fallbackFixtureStatePath(project, runtimeRoot string) string {
	project, _ = filepath.Abs(project)
	if runtime.GOOS == "windows" {
		project = strings.ToLower(project)
	}
	sum := sha256.Sum256([]byte(project))
	return filepath.Join(runtimeRoot, "projects", hex.EncodeToString(sum[:])[:16], "active-coder.json")
}

func writeFallbackFixtureState(t *testing.T, project, runtimeRoot string, input map[string]any) {
	t.Helper()
	state := map[string]any{"active_coder": input["active_coder"], "triggering_evidence": input["triggering_evidence"], "reset_cycle_marker": input["reset_cycle_marker"], "last_probe_time": input["last_probe_time"]}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := fallbackFixtureStatePath(project, runtimeRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readFallbackFixtureState(t *testing.T, project, runtimeRoot string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(fallbackFixtureStatePath(project, runtimeRoot))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
