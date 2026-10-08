package enforce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/profile"
)

func TestPendingCallPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/pending.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string `json:"oracle"`
		Cases  []struct {
			Name     string         `json:"name"`
			Input    map[string]any `json:"input"`
			Expected struct {
				Baseline map[string]any `json:"baseline"`
				Pending  map[string]any `json:"pending"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil || fixture.Oracle != "codex._handoff_matches+ledger.capture_baseline+codex.Pre pending fields" {
		t.Fatalf("fixture: %v %q", err, fixture.Oracle)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			if !strings.HasPrefix(test.Name, "p46_") {
				t.Fatalf("case is not p46: %s", test.Name)
			}
			repo := pendingRepo(t, "base\n")
			worktree := pendingRepo(t, "worktree\n")
			input := test.Input
			var repository *string
			if input["repository"] != nil {
				repository = &repo
			}
			var worktreePath *string
			if input["worktree"] != nil {
				worktreePath = &worktree
			}
			scope := []string{}
			if input["nil_scope"] == true {
				scope = nil
			}
			var live map[string]any
			if mode, _ := input["handoff"].(string); mode != "" {
				post := map[string]any{}
				// Capture the exact pre-handoff state through the same ledger operation.
				baseline, _ := BuildPendingCall(PendingCallInput{Repository: repository, ManifestBudgetSeconds: 3})
				for key, value := range baseline {
					post[key] = value
				}
				if mode == "mismatch" {
					post["post_only"] = true
				}
				live = map[string]any{"origin_baseline": map[string]any{"status": "OK", "origin": test.Name}, "origin_scope": []any{}, "post_state": post, "profile_checks": []any{map[string]any{"name": "inherited"}}}
				if input["live_from_disk"] == true {
					path := filepath.Join(t.TempDir(), "live-thread.json")
					encoded, marshalErr := json.Marshal(live)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					if err = os.WriteFile(path, encoded, 0600); err != nil {
						t.Fatal(err)
					}
					encoded, err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if err = json.Unmarshal(encoded, &live); err != nil {
						t.Fatal(err)
					}
				}
			}
			profileValue := &profile.ResolvedProfile{
				Configured: true, ProtectedPaths: []string{"AGENTS.md"}, ForceFullReview: true,
				SkipMaxLines: 50, SkipMaxFiles: 2, ReviewerCandidates: []profile.ReviewerCandidate{{Agent: "agy", Model: "reviewer-model"}},
				Roles: profile.Roles{Reviewer: "agy"}, AuthoritySources: []profile.AuthoritySource{{Path: "AGENTS.md", Headings: []string{"Authority"}}},
				Checks: []profile.Check{{Name: "build", Argv: []string{"go", "test"}, TimeoutSeconds: 30}}, UserStatus: "ok", ProjectStatus: "ok",
			}
			var rootReason *string
			if value, ok := input["project_root_reason"].(string); ok {
				rootReason = &value
			}
			var priorThread *string
			if input["reply"] == true {
				priorThread = stringPtr("thread-p46")
			}
			call := PendingCallInput{TaskID: "task-p46", ProjectKey: "repo-p46", Phase: "implementation", Route: "operation", Model: "gpt-test", Effort: "high", Label: "pending parity", Agent: "codex", Scope: scope, TaskObjective: "Inspect pending construction", AcceptanceCriteria: "Every field matches Python", TaskContractGrounded: true, TaskType: "operation", ReadinessCheck: map[string]any{"agent": "off"}, Reason: stringPtr("verification"), ReasonSource: stringPtr("explicit"), PriorThreadID: priorThread, ReplyCall: input["reply"] == true, Repository: repository, ProjectRootReason: rootReason, WorktreeReplyPath: worktreePath, ManifestBudgetSeconds: 3, LiveThread: live, Profile: profileValue, MechanicalOpenCodeEnabled: boolPtr(true), Queued: input["queued"] == true}
			baseline, pending := BuildPendingCall(call)
			actual := map[string]any{"baseline": baseline, "pending": pending}
			normalized := normalizePendingPaths(actual, repo, worktree)
			wantBytes, _ := json.Marshal(test.Expected)
			gotBytes, _ := json.Marshal(normalized)
			var want, got any
			_ = json.Unmarshal(wantBytes, &want)
			_ = json.Unmarshal(gotBytes, &got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pending mismatch\n got: %s\nwant: %s", gotBytes, wantBytes)
			}
		})
	}
}

func TestBuildPendingCallSerializesReviewerEffort(t *testing.T) {
	_, pending := BuildPendingCall(PendingCallInput{Profile: &profile.ResolvedProfile{
		Roles:              profile.Roles{Reviewer: "agy"},
		ReviewerCandidates: []profile.ReviewerCandidate{{Agent: "codex", Model: "gpt-6-luna", Effort: "low"}},
	}})
	candidates, ok := pending["reviewer_candidates"].([]map[string]any)
	if !ok || len(candidates) != 1 || candidates[0]["effort"] != "low" {
		t.Fatalf("reviewer candidates=%#v, want codex low effort", pending["reviewer_candidates"])
	}
}

func pendingRepo(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00+00:00")
	t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00+00:00")
	command := func(args ...string) {
		t.Helper()
		cmd := testutil.Hide(exec.Command("git", args...))
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	command("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	command("add", "tracked.txt")
	command("-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	return root
}

func normalizePendingPaths(value any, repo, worktree string) any {
	switch current := value.(type) {
	case string:
		current = strings.ReplaceAll(current, repo, "__REPO__")
		return strings.ReplaceAll(current, worktree, "__WORKTREE__")
	case map[string]any:
		for key, item := range current {
			current[key] = normalizePendingPaths(item, repo, worktree)
		}
	case []any:
		for i, item := range current {
			current[i] = normalizePendingPaths(item, repo, worktree)
		}
	}
	return value
}

func stringPtr(value string) *string { return &value }
func boolPtr(value bool) *bool       { return &value }
