package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"purser-hook/internal/review"
)

func TestBundleNavigationIsBoundedAndKeepsExactRawDiff(t *testing.T) {
	diff := "diff --git a/a b/a\n@@ -1 +1 @@\n-old\n+new\n"
	for _, tc := range []struct {
		name, mode, diff string
		lines            int
		blocked          bool
	}{
		{"eligible", "FULL", diff, 501, false},
		{"control", "FULL", diff, 501, false},
		{"threshold", "FULL", diff, 500, false},
		{"audit", "AUDIT", diff, 501, false},
		{"malformed", "FULL", "diff --git a/a b/a\n@@\n", 501, false},
		{"artifact failure", "FULL", diff, 501, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.blocked {
				if err := os.WriteFile(filepath.Join(root, "review_diffs"), []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			bundle := map[string]any{"claude_review_mode": tc.mode, "delta_lines": tc.lines, "implementer_report": strings.Repeat("report ", 300), "findings": []any{}, "_review_diff": tc.diff}
			event := map[string]any{"session_id": "session", "tool_use_id": "call"}
			key, _ := callKey(event)
			// Choose the navigator cohort without relying on a hard-coded hash.
			for (key[len(key)-1]%2 == 0) != (tc.name == "control") {
				event["tool_use_id"] = stringValue(event["tool_use_id"]) + "x"
				key, _ = callKey(event)
			}
			baseline := clonePostMap(bundle)
			baseline["delta_lines"] = 500
			delete(baseline, "_review_diff")
			base, _, err := finalizePostArtifacts(root, event, baseline, 8000, 5000)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := canonicalJSON(base)
			got, _, err := finalizePostArtifacts(root, event, bundle, 8000, 5000)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := canonicalJSON(got)
			if len(after) > len(before) || got["review_navigation"] != nil || got["_review_diff"] != nil {
				t.Fatalf("delivered bundle grew: %d > %d", len(after), len(before))
			}
			metadata := mapValue(bundle["review_navigation"])
			if tc.mode != "FULL" || tc.lines <= 500 {
				if metadata != nil || got["implementer_report"] != base["implementer_report"] {
					t.Fatal("ineligible presentation changed")
				}
				return
			}
			if tc.blocked || tc.name == "malformed" {
				if metadata["status"] != "UNAVAILABLE" || !strings.HasPrefix(stringValue(got["implementer_report"]), "REVIEW NAVIGATION UNAVAILABLE") {
					t.Fatal("fallback is not visible")
				}
				return
			}
			raw, err := os.ReadFile(stringValue(metadata["raw_diff_path"]))
			index, _ := review.NavigationIndex(string(raw))
			if err != nil || string(raw) != tc.diff || index != metadata["index"] || tc.name != "control" && !strings.HasPrefix(stringValue(got["implementer_report"]), "Review navigation;") {
				t.Fatalf("navigation artifact: %#v, %v", metadata, err)
			}
			if tc.name == "control" && got["implementer_report"] != base["implementer_report"] {
				t.Fatal("control preview changed")
			}
			// Recovery reads the stored artifact after the transient raw material
			// has been removed. It must keep the same presentation and ranges.
			replayed, _, err := finalizePostArtifacts(root, event, bundle, 8000, 5000)
			replayedJSON, _ := canonicalJSON(replayed)
			if err != nil || string(replayedJSON) != string(after) {
				t.Fatalf("recovery changed navigation: %v", err)
			}
		})
	}
}

func TestRecordFixDecisionChecksSourceJobLatestFindings(t *testing.T) {
	current := "F0123456789abcdef-0001"
	for _, tc := range []struct{ name, source, ref, want string }{
		{"unattributed", "job-1", "", "NON_REVIEWER1_OR_UNATTRIBUTED"},
		{"current", "job-1", current, "REVIEWER1_REFERENCED"},
		{"stale", "job-1", "Faaaaaaaaaaaaaaaa-0001", "STALE_OR_CROSS_JOB"},
		{"cross-job", "job-2", current, "STALE_OR_CROSS_JOB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			job := map[string]any{"jobId": "job-1", "taskId": "task-1", "phase": "implementation", "reviewFindingRefs": []any{current}}
			if tc.ref != "" {
				job["fixFindingRefs"] = []any{tc.ref}
			}
			RecordClaudeDecision(root, job, "send_back", "reply", tc.source)
			data, err := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "task-1.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatal(err)
			}
			if mapValue(event["payload"])["review_attribution"] != tc.want {
				t.Fatalf("unverified attribution: %#v", event)
			}
		})
	}
}
