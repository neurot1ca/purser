package review

import "testing"

func TestPythonReviewerGateAndModeAssertions(t *testing.T) {
	operation := Gate("operation", "normal", true, 100, 1000, 2, 50, true)
	empty := Gate("implementation", "normal", true, 0, 0, 2, 50, true)
	small := Gate("implementation", "normal", true, 1, 1, 2, 50, true)
	unavailable := Gate("implementation", "normal", true, 3, 51, 2, 50, false)
	if operation.Reason != PhaseOperation || empty.Reason != NoDelta || small.Reason != BelowThreshold || unavailable.Reason != "REVIEWER_UNAVAILABLE" {
		t.Fatalf("gate skip decisions = %#v %#v %#v %#v", operation, empty, small, unavailable)
	}
	run := Gate("implementation", "normal", true, 3, 51, 2, 50, true)
	operation = Gate("operation", "normal", true, 3, 51, 2, 50, true)
	if !run.Run || run.Reason != "RUN_REVIEWER" || operation.Run || operation.Reason != PhaseOperation {
		t.Fatalf("gate run decisions = %#v %#v", run, operation)
	}
	if Passed(Outcome{ReviewStatus: "COMPLETED", Result: "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", Validation: "PASS"}, empty.Reason) {
		t.Fatal("skipped reviewer became a passing reviewer")
	}
	if got := ReviewMode(true, false, empty.Reason, ""); got.Mode != Audit {
		t.Fatalf("skip review mode = %#v", got)
	}
	if got := Gate("implementation", "normal", true, -1, 10, 2, 50, true); !got.Run || got.Reason != "UNCLASSIFIABLE_INPUT" {
		t.Fatalf("unclassifiable gate input = %#v", got)
	}

	cases := []struct{ route, skipped, result, mode string }{
		{"normal", NoDelta, "", Audit}, {"normal", BelowThreshold, "", Audit},
		{"normal", "REVIEWER_UNAVAILABLE", "", Full}, {"normal", PhaseOperation, "", Full},
		{"normal", "", "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", Audit},
		{"normal", "", "CHANGES_REQUIRED", Full}, {"normal", "", "UNCERTAIN", Full},
		{"normal", "", "REVIEWER1_UNAVAILABLE", Full}, {"consequential", NoDelta, "", Full},
		{"escalated", "", "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE", Full},
	}
	for _, tc := range cases {
		alwaysFull := tc.route == "consequential" || tc.route == "escalated"
		got := ReviewMode(true, alwaysFull, tc.skipped, tc.result)
		if got.Mode != tc.mode {
			t.Errorf("ReviewMode(%s,%s,%s) = %#v, want %s", tc.route, tc.skipped, tc.result, got, tc.mode)
		}
	}
	if got := ReviewMode(true, false, "", "unknown"); got.Mode != Full {
		t.Fatalf("unknown result review mode = %#v", got)
	}
}
