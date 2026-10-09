package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPostArtifactsPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_artifacts.json")
	mustPostTest(t, err)
	var fixture map[string]any
	mustPostTest(t, json.Unmarshal(data, &fixture))
	event := mapValue(fixture["event"])
	root := t.TempDir()
	for _, raw := range fixture["plans"].([]any) {
		plan := mapValue(raw)
		bundle := map[string]any{"findings": []any{}}
		path, storeErr := storePostPlan(root, event, plan["task_id"], plan["report"].(string), bundle)
		mustPostTest(t, storeErr)
		content, readErr := os.ReadFile(path)
		mustPostTest(t, readErr)
		if filepath.Base(filepath.Dir(path)) != "plans" || filepath.Base(path) != plan["filename"] || string(content) != plan["content"] || bundle["plan_summary"] != plan["summary"] || !reflect.DeepEqual(bundle["findings"], plan["findings"]) {
			t.Errorf("plan %s mismatch: path=%s summary=%v findings=%v", plan["name"], path, bundle["plan_summary"], bundle["findings"])
		}
	}
	limits := mapValue(fixture["report_limits"])
	if postReportLimit(nil, "implementation") != int(limits["implementation"].(float64)) ||
		postReportLimit(nil, "operation") != int(limits["operation"].(float64)) ||
		postReportLimit(map[string]any{"operation_report_max_chars": float64(7)}, "operation") != int(limits["custom"].(float64)) ||
		postReportLimit(map[string]any{"implementation_report_max_chars": json.Number("1300")}, "implementation") != 1300 ||
		postReportLimit(map[string]any{"operation_report_max_chars": json.Number("25000")}, "operation") != 25000 {
		t.Fatal("report limits differ from Python")
	}

	quota := mapValue(fixture["quota"])
	bundle := mapValue(quota["bundle"])
	compact, fullPath, err := finalizePostArtifacts(root, event, bundle, int(quota["cap"].(float64)), 7)
	mustPostTest(t, err)
	if compact["implementer_report"] != mapValue(fixture["report_limits"])["truncated"] || compact["codex_quota"] == nil || bundle["full_bundle_path"] != fullPath || filepath.Base(filepath.Dir(fullPath)) != "bundle_artifacts" {
		t.Fatalf("report truncation or quota snapshot missing: %v", compact)
	}
	quotaExpected := mapValue(quota["expected"])
	if !reflect.DeepEqual(compact["codex_quota"], quotaExpected["quota"]) {
		t.Errorf("quota snapshot=%#v want %#v", compact["codex_quota"], quotaExpected["quota"])
	}
	message := mapValue(compact["findings"].([]any)[0])["message"].(string)
	if quotaExpected["message_was_compacted"].(bool) && message == mapValue(bundle["findings"].([]any)[0])["message"] {
		t.Fatalf("quota compaction message differs: %d chars", len([]rune(message)))
	}

	artifacts := mapValue(fixture["artifacts"])
	rawPath, err := persistRawOutputAs(root, artifacts["raw_filename"].(string), artifacts["raw_payload"])
	mustPostTest(t, err)
	rawBytes, err := os.ReadFile(rawPath)
	mustPostTest(t, err)
	wantRaw, err := canonicalJSON(artifacts["raw_payload"])
	mustPostTest(t, err)
	if filepath.Base(filepath.Dir(rawPath)) != "raw_tool_outputs" || filepath.Base(rawPath) != artifacts["raw_filename"] || string(rawBytes) != string(wantRaw) {
		t.Fatalf("raw artifact mismatch: %s %s", rawPath, rawBytes)
	}
	if filepath.Base(fullPath) != artifacts["full_filename"] {
		t.Fatalf("full artifact filename = %s", filepath.Base(fullPath))
	}
	full, err := os.ReadFile(fullPath)
	mustPostTest(t, err)
	var saved map[string]any
	mustPostTest(t, json.Unmarshal(full, &saved))
	for key, expected := range mapValue(artifacts["bundle"]) {
		if key == "full_bundle_path" {
			continue
		}
		if !reflect.DeepEqual(saved[key], expected) {
			t.Errorf("saved bundle[%s]=%#v want %#v", key, saved[key], expected)
		}
	}
}

func TestCompactBundleUsesPlanArtifactWhenMultibyteSummaryExceedsByteBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "complete-plan.md")
	summary := strings.Repeat("界🙂", 1000)
	bundle := map[string]any{
		"plan_path": path, "plan_summary": summary, "findings": []any{}, "implementer_report": "",
	}
	notice := "Complete plan summary is available at " + path + "."
	cap := utf8.RuneCountInString(summary) + 50
	if utf8.RuneCountInString(summary) >= cap || len([]byte(summary)) <= cap {
		t.Fatalf("test does not distinguish rune count from transport bytes: runes=%d bytes=%d cap=%d", utf8.RuneCountInString(summary), len([]byte(summary)), cap)
	}
	compact, overflow, err := compactBundle(bundle, cap)
	mustPostTest(t, err)
	if overflow || compact["plan_summary"] != notice || compact["plan_path"] != path {
		t.Fatalf("compact bundle did not preserve artifact pointer with notice: overflow=%v bundle=%v", overflow, compact)
	}
	if size, sizeErr := nonReportBytes(compact); sizeErr != nil || size > cap {
		t.Fatalf("compact bytes=%d cap=%d err=%v", size, cap, sizeErr)
	}
}

func TestStorePostPlanShapeFinding(t *testing.T) {
	missingReport := "# Summary: Plan\n# Milestones\n- One"
	completeReport := "# Summary: Plan\n# Goal and boundaries\n# Current evidence\n# Decision\n# Milestones\n# Risks/open decisions\n# Implementation handoff"
	for _, test := range []struct {
		name, report string
		wantMissing  []string
	}{
		{name: "missing sections", report: missingReport, wantMissing: []string{"Goal and boundaries", "Current evidence", "Decision", "Risks/open decisions", "Implementation handoff"}},
		{name: "complete plan", report: completeReport},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := map[string]any{"findings": []any{}}
			path, err := storePostPlan(t.TempDir(), map[string]any{"session_id": "s", "tool_use_id": "u"}, "task", test.report, bundle)
			if err != nil || path == "" {
				t.Fatalf("storePostPlan path=%q err=%v", path, err)
			}
			findings := bundle["findings"].([]any)
			var shapeMessage string
			for _, raw := range findings {
				if finding := mapValue(raw); finding["code"] == "PLAN_SHAPE_MISSING" {
					shapeMessage, _ = finding["message"].(string)
				}
			}
			if len(test.wantMissing) == 0 && shapeMessage != "" {
				t.Fatalf("complete plan got PLAN_SHAPE_MISSING: %q", shapeMessage)
			}
			for _, heading := range test.wantMissing {
				if !strings.Contains(shapeMessage, heading) {
					t.Errorf("PLAN_SHAPE_MISSING message %q does not name %q", shapeMessage, heading)
				}
			}
			if len(test.wantMissing) > 0 && shapeMessage == "" {
				t.Fatalf("missing sections were not reported; findings=%#v", findings)
			}
			if got := bundle["plan_summary"]; got != postPlanSummary(test.report) {
				t.Fatalf("plan summary fallback changed: got %#v", got)
			}
		})
	}
}
