package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/review"
)

func TestPostReviewerOutcomePythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_outcome.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name           string         `json:"name"`
			Outcome        string         `json:"outcome"`
			Attributes     map[string]any `json:"attributes"`
			Pending        map[string]any `json:"pending"`
			Bundle         map[string]any `json:"bundle"`
			Delta          map[string]any `json:"delta"`
			Classification map[string]any `json:"classification"`
			Expected       map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			bundle := clonePostMap(test.Bundle)
			applyPostReviewerOutcome(t.TempDir(), map[string]any{"session_id": "fixture", "tool_use_id": test.Name},
				test.Pending, bundle, test.Delta, test.Classification,
				review.BridgeOutcome{Outcome: test.Outcome, Attributes: test.Attributes}, true, false)
			actual := map[string]any{"claude_review_mode": bundle["claude_review_mode"],
				"reviewer1_result": bundle["reviewer1_result"], "reviewer1_skipped_reason": bundle["reviewer1_skipped_reason"],
				"reviewer1_scope": bundle["reviewer1_scope"], "findings": bundle["findings"]}
			actualJSON, _ := json.Marshal(actual)
			wantJSON, _ := json.Marshal(test.Expected)
			if !reflect.DeepEqual(string(actualJSON), string(wantJSON)) {
				t.Fatalf("outcome=%#v want %#v", actual, test.Expected)
			}
		})
	}
}

func TestPostReviewerOutcomeKeepsStringMapFindings(t *testing.T) {
	bundle := map[string]any{"findings": []any{}}
	applyPostReviewerOutcome(t.TempDir(), map[string]any{"session_id": "fixture", "tool_use_id": "typed-findings"},
		map[string]any{}, bundle, map[string]any{}, map[string]any{},
		review.BridgeOutcome{Outcome: "completed", Attributes: map[string]any{
			"result": "CHANGES_REQUIRED", "review_status": "COMPLETED", "validation": "FAIL",
			"findings": []map[string]string{{"code": "REVIEWER_FINDING", "message": "Keep this finding."}},
		}}, true, false)
	findings, ok := bundle["findings"].([]any)
	if !ok || len(findings) != 1 || mapValue(findings[0])["code"] != "REVIEWER_FINDING" || mapValue(findings[0])["message"] != "Keep this finding." {
		t.Fatalf("findings=%#v", bundle["findings"])
	}
}
