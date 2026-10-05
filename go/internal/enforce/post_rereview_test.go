package enforce

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"purser-hook/internal/review"
)

func TestPostRereviewPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_rereview.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string         `json:"name"`
			Input    map[string]any `json:"input"`
			Expected map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			input := tc.Input
			bundle := map[string]any{
				"micro_repair_cleared_review": false, "reviewer1_result": input["reviewer1_result"],
				"reviewer1_skipped_reason": input["reviewer1_skipped_reason"], "reviewer1_provider": input["reviewer1_provider"],
				"reviewer1_model_requested": input["reviewer1_model_requested"], "validation": input["validation"],
				"worker_validation": input["worker_validation"], "hook_checks": input["hook_checks"],
				"thread_id": input["thread_id"], "changed_paths": input["changed_paths"],
				"delta_lines": input["delta_lines"], "delta_files": input["delta_files"],
				"delta": input["delta"], "task_contract_grounded": input["task_contract_grounded"],
				"implementer": input["implementer"], "model": input["model"], "effort": input["effort"], "findings": []any{},
			}
			if input["repair_ran"] == true {
				bundle["micro_repair"] = map[string]any{"attempted": true, "status": "RUNNING"}
				var rereview map[string]any
				encoded, _ := json.Marshal(input["rereview"])
				_ = json.Unmarshal(encoded, &rereview)
				setPostRereview(bundle, review.BridgeOutcome{Outcome: map[bool]string{true: "unavailable", false: "completed"}[rereview["review_status"] == "UNAVAILABLE"], Attributes: rereview})
			}
			if repair, ok := bundle["micro_repair"].(map[string]any); ok {
				repair["status"] = input["repair_status"]
			}
			pending := map[string]any{"task_id": input["task_id"], "route": input["route"], "phase": input["phase"],
				"task_contract_grounded": input["task_contract_grounded"]}
			loaded := map[string]any{"always_full_routes": []any{}}
			finalizePostReview(bundle, pending, loaded, review.GateDecision{Run: true}, nil, true, false, false, false)
			actual := map[string]any{"micro_repair_cleared_review": bundle["micro_repair_cleared_review"],
				"repair_status": nil, "claude_review_mode": bundle["claude_review_mode"], "audit_receipt": bundle["audit_receipt"]}
			if repair, ok := bundle["micro_repair"].(map[string]any); ok {
				actual["repair_status"] = repair["status"]
			}
			if !reflect.DeepEqual(actual, tc.Expected) {
				t.Fatalf("final review = %#v; Python oracle = %#v", actual, tc.Expected)
			}
		})
	}
}
