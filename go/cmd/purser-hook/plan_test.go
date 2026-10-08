package main

import "testing"

func TestPlanMilestoneArguments(t *testing.T) {
	values, err := parseMilestones("1, 3,5")
	if err != nil || len(values) != 3 || values[0] != 1 || values[1] != 3 || values[2] != 5 {
		t.Fatalf("parseMilestones = %v, %v", values, err)
	}
	for _, invalid := range []string{"", "0", "1,x", "-1"} {
		if _, err := parseMilestones(invalid); err == nil {
			t.Errorf("parseMilestones(%q) succeeded", invalid)
		}
	}
}
