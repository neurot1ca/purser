package enforce

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
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
			if findings, ok := input["pre_repair_findings"].([]any); ok {
				bundle["findings"] = findings
			}
			if input["repair_ran"] == true {
				bundle["micro_repair"] = map[string]any{"attempted": true, "status": "RUNNING"}
				var rereview map[string]any
				encoded, _ := json.Marshal(input["rereview"])
				_ = json.Unmarshal(encoded, &rereview)
				if rereview["result"] == "UNCERTAIN" {
					setPostRereview(bundle, review.BridgeOutcome{})
				} else {
					setPostRereview(bundle, review.BridgeOutcome{Outcome: map[bool]string{true: "unavailable", false: "completed"}[rereview["review_status"] == "UNAVAILABLE"], Attributes: rereview})
				}
			}
			pending := map[string]any{"task_id": input["task_id"], "route": input["route"], "phase": input["phase"],
				"task_contract_grounded": input["task_contract_grounded"]}
			loaded := map[string]any{"always_full_routes": []any{}}
			finalizePostReview(bundle, pending, loaded, review.GateDecision{Run: true}, nil, true, false, false, false)
			actual := map[string]any{"micro_repair_cleared_review": bundle["micro_repair_cleared_review"],
				"repair_status": nil, "claude_review_mode": bundle["claude_review_mode"], "audit_receipt": bundle["audit_receipt"],
				"findings": bundle["findings"], "validation": bundle["validation"]}
			if repair, ok := bundle["micro_repair"].(map[string]any); ok {
				actual["repair_status"] = repair["status"]
			}
			if !reflect.DeepEqual(actual, tc.Expected) {
				t.Fatalf("final review = %#v; Python oracle = %#v", actual, tc.Expected)
			}
		})
	}
}

func TestPostRereviewCommitCommandIncludesAllChangedPaths(t *testing.T) {
	paths := []string{
		"file-01.go", "file-02.go", "file-03.go", "file-04.go", "file-05.go", "file-06.go",
		"file-07.go", "file-08.go", "file-09.go", "file-10.go", "file-11.go", "file-12.go",
	}
	receipt := buildPostAuditReceipt(map[string]any{"changed_paths": paths}, "task-12")

	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = `"` + path + `"`
	}
	want := `git add ` + strings.Join(quoted, " ") + ` && git commit -m "feat(task-12): implement changes"`
	if got := receipt["commit_command"]; got != want {
		t.Fatalf("commit_command = %q; want %q", got, want)
	}
}
