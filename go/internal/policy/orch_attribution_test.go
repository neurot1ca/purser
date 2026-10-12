package policy

import "testing"

func TestFixFindingMetadataIsStrippedAndValidated(t *testing.T) {
	config := policyTestConfig(t)
	base := "ORCH|task=t|phase=implementation|route=subtle|scope=go/a.go|reason=fix"
	ref := "F0123456789abcdef-0001"
	got := ParseOrchPrompt(base+"|review_findings="+ref+"\nFix the issue.", config)
	if !got.Valid || got.Prompt != "Fix the issue." || len(got.ReviewFindings) != 1 || got.ReviewFindings[0] != ref {
		t.Fatalf("routing metadata leaked or lost: %#v", got)
	}
	for _, raw := range []string{"", "invalid", ref + "," + ref} {
		if got := ParseOrchPrompt(base+"|review_findings="+raw+"\nFix.", config); got.Valid {
			t.Fatalf("invalid references accepted: %q", raw)
		}
	}
}
