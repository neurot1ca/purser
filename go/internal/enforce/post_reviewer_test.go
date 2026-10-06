package enforce

import (
	"encoding/json"
	"testing"
	"time"

	"purser-hook/internal/profile"
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
