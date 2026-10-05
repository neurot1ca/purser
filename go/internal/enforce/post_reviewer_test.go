package enforce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/profile"
)

func TestPostReviewerFamilyAndBoardZeroParity(t *testing.T) {
	for _, tc := range []struct {
		model, family string
	}{
		{"gpt-5", "openai"}, {"openai/gpt-5", "openai"}, {"claude-sonnet", "anthropic"},
		{"anthropic/claude-sonnet", "anthropic"}, {"gemini-3.8-flash-high", "google"},
	} {
		if got := vendorFamily(modelFamily(tc.model)); got != tc.family {
			t.Errorf("family(%q)=%q want %q", tc.model, got, tc.family)
		}
	}
	for _, value := range []any{json.Number("0"), json.Number("0.0"), false} {
		if !boardDisplayZero(value) {
			t.Errorf("boardDisplayZero(%v) = false", value)
		}
	}
	for _, value := range []any{json.Number("1"), true, "0", nil} {
		if boardDisplayZero(value) {
			t.Errorf("boardDisplayZero(%v) = true", value)
		}
	}
}

func TestRunPostReviewerPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_reviewer.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name           string                      `json:"name"`
			Candidates     []profile.ReviewerCandidate `json:"candidates"`
			FallbackFamily string                      `json:"fallback_family"`
			Responses      []string                    `json:"responses"`
			Calls          []struct {
				Agent string `json:"agent"`
				Model string `json:"model"`
			} `json:"calls"`
			Attempts     []postReviewerAttempt `json:"attempts"`
			Skipped      string                `json:"skipped_reason"`
			FindingCodes []string              `json:"finding_codes"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := reviewerFixtureRoot(t)
			trace := filepath.Join(root, "calls.jsonl")
			responses := map[string]string{}
			for i, call := range tc.Calls {
				if i < len(tc.Responses) {
					response := tc.Responses[i]
					if response == "slow_unavailable" {
						response = "unavailable"
					}
					responses[call.Model] = response
				}
			}
			encoded, err := json.Marshal(map[string]any{"responses": responses, "trace": trace})
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "fixture.json")
			if err := os.WriteFile(config, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PURser_REVIEWER_FIXTURE", config)
			pending := map[string]any{"task_id": "fixture", "phase": "implementation", "route": "normal", "model": "gpt-5", "effort": "high", "repo_root": root, "fallback_family": tc.FallbackFamily}
			bundle := map[string]any{"thread_id": "fixture-thread", "worker_validation": map[string]any{"status": "PASS", "summary": "fixture"}, "findings": []any{}}
			resolved := postProfile{ReviewerCandidates: tc.Candidates}
			loaded := map[string]any{"reviewer1_timeout_seconds": 5.0, "reviewer1_model_requested": "gemini-3.8-flash-high"}
			if tc.Name == "deadline_across_candidates" {
				past := time.Now().Add(-time.Second)
				_, err := runPostReviewer(root, root, pending, bundle, resolved, loaded, map[string]any{"status": "OK"}, nil, past, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				attempts, _ := bundle["reviewer1_attempts"].([]postReviewerAttempt)
				if len(attempts) != 2 || attempts[0].Outcome != "UNAVAILABLE_BUDGET_EXHAUSTED" || attempts[1].Outcome != "UNAVAILABLE_BUDGET_EXHAUSTED" {
					t.Fatalf("expired shared deadline attempts=%#v", attempts)
				}
				return
			}
			outcome, callErr := runPostReviewer(root, root, pending, bundle, resolved, loaded, map[string]any{"status": "OK"}, nil, time.Now(), 5*time.Second)
			if tc.Name == "bridge_exception" {
				if callErr == nil || !reflect.DeepEqual(bundle["findings"], []any{map[string]any{"code": "INTERNAL_POST_ERROR", "message": "Reviewer-1 bridge raised unexpectedly."}}) {
					t.Fatalf("exception mapping: outcome=%#v err=%v findings=%#v", outcome, callErr, bundle["findings"])
				}
				return
			}
			if callErr != nil {
				t.Fatal(callErr)
			}
			if tc.Name == "all_filtered" {
				if bundle["reviewer1_skipped_reason"] != "REVIEWER_VENDOR_CONFLICT" || outcome.Outcome != "unavailable" {
					t.Fatalf("filtered result=%#v bundle=%#v", outcome, bundle)
				}
				if !reflect.DeepEqual(tc.FindingCodes, []string{"REVIEWER_VENDOR_CONFLICT"}) {
					t.Fatalf("Python fixture skipped finding=%v", tc.FindingCodes)
				}
				return
			}
			gotTrace, _ := os.ReadFile(trace)
			traceText := strings.ReplaceAll(string(gotTrace), "\r\n", "\n")
			gotCalls := []string{}
			for _, call := range tc.Calls {
				gotCalls = append(gotCalls, call.Agent+":"+call.Model)
			}
			if !strings.Contains(traceText, strings.Join(gotCalls, "\n")) && len(gotCalls) > 0 {
				t.Fatalf("bridge calls %q do not contain %v", traceText, gotCalls)
			}
			if len(tc.Attempts) > 1 && !reflect.DeepEqual(bundle["reviewer1_attempts"], tc.Attempts) {
				t.Fatalf("attempts=%#v want %#v", bundle["reviewer1_attempts"], tc.Attempts)
			}
			wantOutcome := "completed"
			if len(tc.Responses) > 0 && tc.Responses[len(tc.Responses)-1] == "unavailable" {
				wantOutcome = "unavailable"
			}
			if outcome.Outcome != wantOutcome {
				t.Fatalf("bridge outcome=%q want %q", outcome.Outcome, wantOutcome)
			}
		})
	}
}

func reviewerFixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bridge := filepath.Join(root, ".claude", "hooks", "orchestrator", "reviewer_bridge.py")
	locator := filepath.Join(root, "bin", "python-locator.js")
	if err := os.MkdirAll(filepath.Dir(bridge), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(locator), 0700); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("python is unavailable for bridge fixture")
	}
	quoted, _ := json.Marshal(python)
	if err := os.WriteFile(locator, []byte("console.log(JSON.stringify({path: "+string(quoted)+"}));\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := "import json,os,sys\nc=json.load(open(os.environ['PURser_REVIEWER_FIXTURE'],encoding='utf-8'))\na=json.load(sys.stdin)\nm=a['reviewer_model']\nwith open(c['trace'],'a',encoding='utf-8') as f: f.write(a['reviewer_agent']+':'+m+'\\n')\nr=c['responses'].get(m,'completed')\nif r=='exception': o={'outcome':'unavailable','reason':'REVIEWER_BRIDGE_EXCEPTION','error_code':'REVIEWER_BRIDGE_EXCEPTION'}\nelif r=='unavailable': o={'outcome':'unavailable','review_status':'UNAVAILABLE','provider':a['reviewer_agent'],'reviewer1_model_requested':m,'result':'REVIEWER1_UNAVAILABLE','validation':'UNKNOWN','findings':[]}\nelse: o={'outcome':'completed','review_status':'COMPLETED','provider':a['reviewer_agent'],'reviewer1_model_requested':m,'result':'NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE','validation':'PASS','findings':[]}\nprint(json.dumps(o))\n"
	if err := os.WriteFile(bridge, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}
