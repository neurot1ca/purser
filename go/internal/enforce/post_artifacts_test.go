package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
		postReportLimit(map[string]any{"operation_report_max_chars": float64(7)}, "operation") != int(limits["custom"].(float64)) {
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
