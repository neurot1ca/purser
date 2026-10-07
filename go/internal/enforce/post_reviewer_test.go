package enforce

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"purser-hook/internal/profile"
	"purser-hook/internal/review"
)

func TestPostReviewerFamilyAndBoardZeroParity(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
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

func TestRunPostReviewerSameFamilyAndDeadline(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	candidates := []profile.ReviewerCandidate{{Agent: "agy", Model: "gemini-3.8-flash-high"}, {Agent: "opencode", Model: "google/gemini-2.5-pro"}}
	pending := map[string]any{"fallback_family": "google", "model": "gpt-5"}
	bundle := map[string]any{"findings": []any{}}
	loaded := map[string]any{"reviewer1_model_requested": "gemini-3.8-flash-high"}
	outcome, err := runPostReviewer("", "", pending, bundle, postProfile{ReviewerCandidates: candidates}, loaded, nil, nil, time.Now(), time.Second)
	if err != nil || outcome.Outcome != "unavailable" || bundle["reviewer1_skipped_reason"] != "REVIEWER_VENDOR_CONFLICT" {
		t.Fatalf("same-family rejection outcome=%#v err=%v bundle=%#v", outcome, err, bundle)
	}
	attempts, ok := bundle["reviewer1_attempts"].([]postReviewerAttempt)
	if !ok || len(attempts) != 2 || attempts[0].Outcome != "SKIPPED_SAME_VENDOR" || attempts[1].Outcome != "SKIPPED_SAME_VENDOR" {
		t.Fatalf("same-family candidate order=%#v", bundle["reviewer1_attempts"])
	}

	deadlineBundle := map[string]any{"findings": []any{}}
	pending["fallback_family"] = "openai"
	_, err = runPostReviewer("", "", pending, deadlineBundle, postProfile{ReviewerCandidates: candidates}, loaded, nil, nil, time.Now().Add(-time.Second), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadlineAttempts, ok := deadlineBundle["reviewer1_attempts"].([]postReviewerAttempt)
	if !ok || len(deadlineAttempts) != 2 || deadlineAttempts[0].Outcome != "UNAVAILABLE_BUDGET_EXHAUSTED" || deadlineAttempts[1].Outcome != "UNAVAILABLE_BUDGET_EXHAUSTED" {
		t.Fatalf("expired shared deadline attempts=%#v", deadlineBundle["reviewer1_attempts"])
	}
}

func TestClaudeFallbackCannotPassClaudeReviewer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	bundle := map[string]any{"findings": []any{}}
	pending := map[string]any{"agent": "claude", "model": "claude-sonnet-4-5", "fallback_family": "anthropic"}
	candidate := profile.ReviewerCandidate{Agent: "claude", Model: "claude-opus-4-1", Effort: "high"}
	outcome, err := runPostReviewer("", "", pending, bundle, postProfile{ReviewerCandidates: []profile.ReviewerCandidate{candidate}}, map[string]any{}, nil, nil, time.Now(), time.Second)
	if err != nil || outcome.Outcome != "unavailable" || bundle["reviewer1_skipped_reason"] != "REVIEWER_VENDOR_CONFLICT" {
		t.Fatalf("same-family Claude review=%#v err=%v bundle=%#v", outcome, err, bundle)
	}
}

func TestRunPostReviewerUsesConfiguredJSONNumberTimeout(t *testing.T) {
	pluginRoot := t.TempDir()
	configDir := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	config := map[string]any{}
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatal(err)
	}
	config["reviewer1_timeout_seconds"] = 180
	configBytes, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	loaded, err := loadPreConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded["reviewer1_timeout_seconds"].(json.Number); !ok {
		t.Fatalf("loaded timeout type=%T, want json.Number", loaded["reviewer1_timeout_seconds"])
	}
	for key, want := range map[string]float64{
		"hook_checks_deadline_seconds":  150,
		"max_total_hook_budget_seconds": 240,
		"manifest_budget_seconds":       3,
		"micro_repair_timeout_seconds":  60,
	} {
		t.Run(key, func(t *testing.T) {
			if got := numberOr(loaded[key], -1); got != want {
				t.Fatalf("numberOr(%s)=%v, want %v", key, got, want)
			}
		})
	}

	oldBridge := postReviewerBridge
	t.Cleanup(func() { postReviewerBridge = oldBridge })
	var gotBudgets []time.Duration
	var gotTimeouts []float64
	postReviewerBridge = func(_ context.Context, _ string, args map[string]any, timeout time.Duration) (review.BridgeOutcome, error) {
		gotBudgets = append(gotBudgets, timeout)
		gotTimeouts = append(gotTimeouts, args["reviewer_timeout_seconds"].(float64))
		return review.BridgeOutcome{Outcome: "unavailable"}, nil
	}
	candidates := []profile.ReviewerCandidate{{Agent: "agy", Model: "gemini-3.8-flash-high"}}
	for _, tc := range []struct {
		budget time.Duration
		want   time.Duration
	}{{240 * time.Second, 180 * time.Second}, {120 * time.Second, 120 * time.Second}} {
		_, err := runPostReviewer("", "", map[string]any{"model": "gpt-5"}, map[string]any{"findings": []any{}},
			postProfile{ReviewerCandidates: candidates}, loaded, nil, nil, time.Now(), tc.budget)
		if err != nil {
			t.Fatal(err)
		}
		got := gotBudgets[len(gotBudgets)-1]
		if delta := got - tc.want; delta < -time.Second || delta > 0 {
			t.Errorf("reviewer bridge budget=%s, want %s capped by hook budget", got, tc.want)
		}
		if gotTimeout := gotTimeouts[len(gotTimeouts)-1]; gotTimeout > tc.want.Seconds() || gotTimeout < tc.want.Seconds()-1 {
			t.Errorf("reviewer_timeout_seconds=%v, want %v capped by hook budget", gotTimeout, tc.want.Seconds())
		}
	}
}
