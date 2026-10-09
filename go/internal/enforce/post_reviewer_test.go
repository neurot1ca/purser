package enforce

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	pending := map[string]any{"agent": "claude", "model": "sonnet", "fallback_family": "anthropic"}
	candidate := profile.ReviewerCandidate{Agent: "claude", Model: "opus", Effort: "high"}
	outcome, err := runPostReviewer("", "", pending, bundle, postProfile{ReviewerCandidates: []profile.ReviewerCandidate{candidate}}, map[string]any{}, nil, nil, time.Now(), time.Second)
	if err != nil || outcome.Outcome != "unavailable" || bundle["reviewer1_skipped_reason"] != "REVIEWER_VENDOR_CONFLICT" {
		t.Fatalf("same-family Claude review=%#v err=%v bundle=%#v", outcome, err, bundle)
	}
}

func TestReviewerCandidateEffortReachesBridge(t *testing.T) {
	oldBridge := postReviewerBridge
	t.Cleanup(func() { postReviewerBridge = oldBridge })
	var gotEffort any
	postReviewerBridge = func(_ context.Context, _ string, args map[string]any, _ time.Duration) (review.BridgeOutcome, error) {
		gotEffort = args["reviewer_effort"]
		return review.BridgeOutcome{Outcome: "completed"}, nil
	}
	pendingProfile := map[string]any{
		"protected_paths": []any{}, "force_full_review": false, "skip_max_lines": float64(50),
		"skip_max_files": float64(2), "configured": true, "user_status": "ok", "project_status": "ok",
		"reviewer_candidates": []any{map[string]any{"agent": "codex", "model": "gpt-6-luna", "effort": "low"}},
	}
	_, err := runPostReviewer("", "", map[string]any{"fallback_family": "google", "model": "gemini-3.8-flash-high"},
		map[string]any{"findings": []any{}},
		postProfileFromPending(pendingProfile, map[string]any{}),
		map[string]any{"reviewer1_timeout_seconds": float64(1)}, nil, nil, time.Now(), time.Second)
	if err != nil || gotEffort != "low" {
		t.Fatalf("bridge effort=%v err=%v, want low", gotEffort, err)
	}
}

func TestDefaultReviewerFallsBackWithinReservedBudget(t *testing.T) {
	oldBridge := postReviewerBridge
	t.Cleanup(func() { postReviewerBridge = oldBridge })
	var agents []string
	var budgets []time.Duration
	postReviewerBridge = func(_ context.Context, _ string, args map[string]any, timeout time.Duration) (review.BridgeOutcome, error) {
		agents = append(agents, args["reviewer_agent"].(string))
		budgets = append(budgets, timeout)
		if len(agents) == 1 {
			time.Sleep(timeout)
			return review.BridgeOutcome{Outcome: "unavailable"}, nil
		}
		return review.BridgeOutcome{Outcome: "completed", Attributes: map[string]any{"result": "fallback-used"}}, nil
	}
	loaded := map[string]any{"reviewer1_model_requested": "gemini-3.8-flash-high", "reviewer1_timeout_seconds": float64(2)}
	bundle := map[string]any{"findings": []any{}}
	outcome, err := runPostReviewer("", "", map[string]any{"model": "gpt-6-luna", "fallback_family": "openai"}, bundle,
		postProfile{}, loaded, nil, nil, time.Now(), 2*time.Second)
	if err != nil || outcome.Attributes["result"] != "fallback-used" {
		t.Fatalf("fallback outcome=%#v err=%v", outcome, err)
	}
	if len(agents) != 2 || agents[0] != "agy" || agents[1] != "claude" {
		t.Fatalf("reviewer calls=%v, want [agy claude]", agents)
	}
	if budgets[0] > 1300*time.Millisecond || budgets[1] < 500*time.Millisecond {
		t.Fatalf("budgets=%v, want first capped near 60%% and fallback reserved", budgets)
	}
	attempts, ok := bundle["reviewer1_attempts"].([]postReviewerAttempt)
	if !ok || len(attempts) != 2 || attempts[0].Outcome != "UNAVAILABLE" || attempts[1].Outcome != "COMPLETED" {
		t.Fatalf("attempts=%#v", bundle["reviewer1_attempts"])
	}
}

func TestPostReviewerClaudeFallbackEffortAndReason(t *testing.T) {
	oldBridge := postReviewerBridge
	t.Cleanup(func() { postReviewerBridge = oldBridge })
	var claudeEffort any
	postReviewerBridge = func(_ context.Context, _ string, args map[string]any, _ time.Duration) (review.BridgeOutcome, error) {
		if args["reviewer_agent"] == "agy" {
			return review.BridgeOutcome{Outcome: "unavailable", Attributes: map[string]any{
				"error_code": "AGY_DOWN", "findings": []any{map[string]any{"code": "AGY_DOWN", "message": "provider unavailable"}},
			}}, nil
		}
		claudeEffort = args["reviewer_effort"]
		return review.BridgeOutcome{Outcome: "completed"}, nil
	}
	candidates := []profile.ReviewerCandidate{
		{Agent: "agy", Model: "gemini-3.8-flash-high"},
		{Agent: "claude", Model: "claude-haiku-5-5"},
	}
	bundle := map[string]any{"findings": []any{}}
	_, err := runPostReviewer("", "", map[string]any{"model": "gpt-6-luna", "fallback_family": "openai"}, bundle,
		postProfile{ReviewerCandidates: candidates}, map[string]any{"reviewer1_effort": "high"}, nil, nil, time.Now(), 2*time.Second)
	if err != nil || claudeEffort != "high" {
		t.Fatalf("Claude fallback effort=%v err=%v, want configured high", claudeEffort, err)
	}
	attempts, ok := bundle["reviewer1_attempts"].([]postReviewerAttempt)
	if !ok || len(attempts) != 2 || attempts[0].Reason != "AGY_DOWN: provider unavailable" {
		t.Fatalf("attempts=%#v, want bounded unavailable reason", bundle["reviewer1_attempts"])
	}
}

func TestPostReviewerClaudeFallbackRunsWithDefaultEffortFakeExecutable(t *testing.T) {
	pluginRoot := t.TempDir()
	repo := filepath.Join(pluginRoot, "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	postReviewRepo(t, repo, map[string]any{"changed_paths": []any{"change.txt"}})
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("changed after baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	baseline, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	source := `package main
import ("fmt"; "os")
func main() {
  effort := ""
  for i, arg := range os.Args { if arg == "--effort" && i+1 < len(os.Args) { effort = os.Args[i+1] } }
  if effort == "medium" { fmt.Print("{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"fake-claude\",\"structured_output\":{\"result\":\"NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE\",\"validation\":\"PASS\",\"findings\":[]}}") } else { fmt.Print("invalid") }
}`
	sourcePath := filepath.Join(pluginRoot, "fake_claude.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(pluginRoot, "fake-claude")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	if err := exec.Command("go", "build", "-o", executable, sourcePath).Run(); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"reviewer1_model_requested":"gemini-3.8-flash-high","reviewer1_timeout_seconds":20,"reviewer1_packet_max_bytes":200000}`), 0600); err != nil {
		t.Fatal(err)
	}
	candidates := []profile.ReviewerCandidate{
		{Agent: "agy", Model: "gemini-3.8-flash-high"},
		{Agent: "claude", Model: "claude-haiku-5-5"},
	}
	bundle := map[string]any{"findings": []any{}}
	outcome, err := runPostReviewer(pluginRoot, "", map[string]any{
		"task_id": "fake-executable-test", "phase": "implementation", "route": "normal",
		"model": "gpt-6-luna", "effort": "medium", "fallback_family": "openai", "repo_root": repo,
		"delta": map[string]any{"changed_paths": []any{"change.txt"}, "baseline_head": strings.TrimSpace(string(baseline)),
			"ambiguous_paths": []any{}, "changed_file_count": 1, "changed_line_count": 1,
			"sources": map[string]any{"scoped_manifest": map[string]any{"paths": []any{}}}},
	}, bundle, postProfile{ReviewerExecutable: executable, ReviewerCandidates: candidates},
		map[string]any{"reviewer1_model_requested": "gemini-3.8-flash-high", "reviewer1_timeout_seconds": 20.0},
		map[string]any{"changed_paths": []any{"change.txt"}, "baseline_head": strings.TrimSpace(string(baseline)),
			"ambiguous_paths": []any{}, "changed_file_count": 1, "changed_line_count": 1,
			"sources": map[string]any{"scoped_manifest": map[string]any{"paths": []any{}}}}, nil, time.Now(), 20*time.Second)
	if err != nil || outcome.Outcome != "completed" {
		t.Fatalf("fallback outcome=%#v err=%v", outcome, err)
	}
	attempts, ok := bundle["reviewer1_attempts"].([]postReviewerAttempt)
	if !ok || len(attempts) != 2 || attempts[0].Outcome != "UNAVAILABLE" || !strings.HasPrefix(attempts[0].Reason, review.ResponseInvalid+": ") || attempts[1].Outcome != "COMPLETED" {
		t.Fatalf("attempts=%#v", bundle["reviewer1_attempts"])
	}
}

func TestDefaultReviewerSkipsImplementerVendorFallback(t *testing.T) {
	oldBridge := postReviewerBridge
	t.Cleanup(func() { postReviewerBridge = oldBridge })
	postReviewerBridge = func(_ context.Context, _ string, _ map[string]any, _ time.Duration) (review.BridgeOutcome, error) {
		return review.BridgeOutcome{Outcome: "unavailable"}, nil
	}
	loaded := map[string]any{"reviewer1_model_requested": "gemini-3.8-flash-high", "reviewer1_timeout_seconds": float64(1)}
	for _, tc := range []struct {
		family string
		vendor string
	}{
		{family: "openai", vendor: "openai"},
		{family: "anthropic", vendor: "anthropic"},
	} {
		bundle := map[string]any{"findings": []any{}}
		_, err := runPostReviewer("", "", map[string]any{"fallback_family": tc.family}, bundle,
			postProfile{}, loaded, nil, nil, time.Now(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		attempts, ok := bundle["reviewer1_attempts"].([]postReviewerAttempt)
		if !ok {
			t.Fatalf("family %s attempts missing: %#v", tc.family, bundle)
		}
		found := false
		for _, attempt := range attempts {
			if attempt.Agent == map[string]string{"openai": "codex", "anthropic": "claude"}[tc.vendor] {
				found = attempt.Outcome == "SKIPPED_SAME_VENDOR"
			}
		}
		if !found {
			t.Fatalf("family %s did not skip its vendor candidate: %#v", tc.family, attempts)
		}
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
