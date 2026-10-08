package profile

import (
	"fmt"
	"strings"
	"testing"
)

func TestMaintenanceCoverageTargetValidation(t *testing.T) {
	for _, test := range []struct {
		value string
		want  float64
		warn  bool
	}{{"80", 80, false}, {"0", 90, true}, {"150", 90, true}, {`"x"`, 90, true}} {
		t.Run(test.value, func(t *testing.T) {
			tier := ValidateTierTOML("project", []byte(fmt.Sprintf("schema_version = 1\n[maintenance]\ncoverage_target_pct = %s\n", test.value)))
			got := ResolveProfile(".", Tier{Status: "missing", Values: map[string]any{}}, tier)
			if tier.Status != "ok" || got.CoverageTargetPct != test.want {
				t.Fatalf("tier=%#v target=%v, want %v", tier, got.CoverageTargetPct, test.want)
			}
			if test.warn != (len(got.Warnings) == 1 && strings.Contains(got.Warnings[0], "coverage_target_pct")) {
				t.Fatalf("warnings = %#v", got.Warnings)
			}
		})
	}
}
