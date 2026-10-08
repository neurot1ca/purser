package contextio

import (
	"strings"
	"testing"
)

func TestMaintenanceNoticeCoverageTargetAndFindingOrder(t *testing.T) {
	for _, test := range []struct {
		name      string
		coverage  any
		target    any
		celebrate bool
	}{{"override boundary", float64(80), float64(80), true}, {"below override", float64(79.9), float64(80), false}, {"null coverage", nil, float64(80), false}, {"legacy boundary", float64(90), nil, true}} {
		t.Run(test.name, func(t *testing.T) {
			notice := map[string]any{"report_type": "test-gaps", "finding_count": 0, "findings": []any{}}
			if test.coverage != nil {
				notice["line_coverage_pct"] = test.coverage
			}
			if test.target != nil {
				notice["coverage_target_pct"] = test.target
			}
			got := strings.Contains(formatMaintenanceNotice(notice), "code is now")
			if got != test.celebrate {
				t.Fatalf("celebration=%v, want %v", got, test.celebrate)
			}
		})
	}
	findings := []any{
		map[string]any{"path_line": "a.py:1", "symbol": "first", "evidence": "e", "confidence": "low", "confidence_rank": 1},
		map[string]any{"path_line": "b.py:2", "symbol": "second", "evidence": "e", "confidence": "high", "confidence_rank": 3},
	}
	for _, test := range []struct {
		reportType string
		first      string
		second     string
	}{{"test-gaps", "a.py:1 first", "b.py:2 second"}, {"cleanup", "b.py:2 second", "a.py:1 first"}} {
		text := formatMaintenanceNotice(map[string]any{"report_type": test.reportType, "finding_count": 2, "findings": findings})
		first, second := strings.Index(text, test.first), strings.Index(text, test.second)
		if first < 0 || second < 0 || first > second {
			t.Fatalf("%s notice order mismatch for %q before %q: %s", test.reportType, test.first, test.second, text)
		}
	}
}

func TestMaintenanceNoticeWhyColumnsAndLegacyFindings(t *testing.T) {
	master := `> *Say "turn off maintenance" to stop these reports, or run one any time with /purser:optimize.*`
	for _, test := range []struct {
		reportType string
		header     string
	}{
		{"cleanup", "> | Finding | Why clean it | Evidence |"},
		{"test-gaps", "> | Gap | Why it matters | Evidence |"},
	} {
		t.Run(test.reportType+" legacy", func(t *testing.T) {
			text := formatMaintenanceNotice(map[string]any{
				"report_type":   test.reportType,
				"finding_count": 1,
				"findings":      []any{map[string]any{"path_line": "a.py:1", "symbol": "a", "evidence": "e", "confidence": "high"}},
			})
			if !strings.Contains(text, test.header) || !strings.Contains(text, "| a.py:1 a | — | e |") || !strings.Contains(text, master) {
				t.Fatalf("legacy notice missing reason column or master instruction: %s", text)
			}
		})
		t.Run(test.reportType+" why truncation", func(t *testing.T) {
			why := strings.Repeat("界", 70)
			evidence := strings.Repeat("e", 60)
			text := formatMaintenanceNotice(map[string]any{
				"report_type":   test.reportType,
				"finding_count": 1,
				"findings":      []any{map[string]any{"path_line": "a.py:1", "symbol": "a", "why": why, "evidence": evidence, "confidence": "high"}},
			})
			row := "| a.py:1 a | " + strings.Repeat("界", 64) + " | " + strings.Repeat("e", 48) + " |"
			if !strings.Contains(text, row) {
				t.Fatalf("notice did not apply rune limits: %s", text)
			}
		})
	}
}
