package maintenance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestReportPromptBytes(t *testing.T) {
	repo := "<REPO>"
	cleanup, err := BuildReportPrompt(repo, "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	wantCleanup := "Produce a cleanup report for the repository at <REPO>.\n" +
		"This is a strictly read-only inspection. Do not edit files, create repository files,\n" +
		"install tools, or run package managers. Report only evidence-backed candidates.\n" +
		"For every item include (1) exact tool output or search evidence, (2) repository\n" +
		"path and symbol, and (3) checks against entry points, hooks, and dynamic/reflection\n" +
		"use before calling it unused. If vulture is available, use its installed version;\n" +
		"otherwise state that vulture is unavailable and use grep-based searches, explaining\n" +
		"that grep can miss indirect or dynamic use. Never install any tool. Include the\n" +
		"commands/searches used and distinguish uncertain candidates from confirmed facts.\n" +
		"If user-initiated work starts, it has priority: do not delay it, cancel it, or\n" +
		"continue consuming resources once it needs the machine. End your report with this\n" +
		"machine-readable list, using one object per finding and an empty list for no findings:\n" +
		"```purser-findings-json\n" +
		"[{\"path_line\":\"relative/path.py:12\",\"symbol\":\"name\",\"evidence\":\"short evidence\",\"confidence\":\"high|medium|low\"}]\n```\n"
	if cleanup != wantCleanup {
		t.Fatalf("cleanup prompt mismatch\n got: %q\nwant: %q", cleanup, wantCleanup)
	}
	testGaps, err := BuildReportPrompt(repo, "test-gaps")
	if err != nil {
		t.Fatal(err)
	}
	wantTestGaps := "Produce a test-gaps report for the repository at <REPO>.\n" +
		"This is a strictly read-only inspection. Do not edit files, create repository files,\n" +
		"install tools, or run package managers. Report only evidence-bound candidate tests.\n" +
		"For each candidate, give the target path:line and symbol, the untested behaviour,\n" +
		"evidence from source, the exact command used to search existing tests and its result\n" +
		"showing no test already covers that behaviour, and a high, medium, or low confidence\n" +
		"level. Include a compact contract evidence section covering documented requirements,\n" +
		"invariants, boundaries, and affected callers, cite each item to a source path:line,\n" +
		"and write \"unknown\" where evidence is absent.\n" +
		"Check whether a coverage tool is already available in this project's environment.\n" +
		"For Python coverage, if `python -m coverage --version` succeeds, run\n" +
		"`python -m coverage report --format=total`; report its total line coverage as a\n" +
		"number and include that exact command. Use an existing configured command for other\n" +
		"available coverage tools. If no usable coverage tool or coverage data is available,\n" +
		"report coverage as unknown. Never install anything.\n" +
		"If user-initiated work starts, it has priority: do not delay it, cancel it, or\n" +
		"continue consuming resources once it needs the machine. End with one JSON object in\n" +
		"this machine-readable block. Findings use the cleanup shape; line_coverage_pct must\n" +
		"be a number or null:\n```purser-findings-json\n" +
		"{\"findings\":[{\"path_line\":\"relative/path.py:12\",\"symbol\":\"name\",\"evidence\":\"source evidence; untested behaviour; test search command and no-match result\",\"confidence\":\"high|medium|low\"}],\"line_coverage_pct\":null}\n```\n"
	if testGaps != wantTestGaps {
		t.Fatalf("test-gaps prompt mismatch\n got: %q\nwant: %q", testGaps, wantTestGaps)
	}
}

func TestFindingsParsingOracle(t *testing.T) {
	cases := []struct {
		name, body string
		count      int
		coverage   any
	}{
		{"well-formed", "```purser-findings-json\n{\"findings\":[{\"path_line\":\"a.py:1\",\"symbol\":\"a\",\"evidence\":\"e\",\"confidence\":\"low\"},{\"path_line\":\"b.py:2\",\"symbol\":\"b\",\"evidence\":\"e\",\"confidence\":\"high\"}],\"line_coverage_pct\":73.5}\n```", 2, json.Number("73.5")},
		{"empty", "```purser-findings-json\n[]\n```", 0, nil},
		{"malformed", "```purser-findings-json\n{bad}\n```", -1, nil},
		{"partial-coverage", "```purser-findings-json\n{\"findings\":[{\"path_line\":\"a.py:1\",\"symbol\":\"a\",\"evidence\":\"e\",\"confidence\":\"high\"}]}\n```", -1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseFindings(tc.body)
			if tc.count < 0 {
				if got != nil {
					t.Fatalf("findings = %#v, want unknown", got)
				}
			} else if got == nil || len(got) != tc.count {
				t.Fatalf("findings = %#v", got)
			}
			if !reflect.DeepEqual(LineCoverage(tc.body), tc.coverage) {
				t.Fatalf("coverage = %#v, want %#v", LineCoverage(tc.body), tc.coverage)
			}
			if tc.name == "well-formed" && got[0]["confidence"] != "high" {
				t.Fatalf("confidence order = %#v", got)
			}
		})
	}
}

func TestRunAndPersistReportSuccessAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			caseDir := maintenanceScratch(t)
			codexHome := filepath.Join(caseDir, "codex-home")
			if err := os.MkdirAll(codexHome, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", codexHome)
			state, repo := filepath.Join(caseDir, "state"), filepath.Join(caseDir, "repo")
			if err := os.MkdirAll(state, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(repo, 0700); err != nil {
				t.Fatal(err)
			}
			result := map[string]any{"state": "SUCCEEDED", "content": "Body.\n```purser-findings-json\n{\"findings\":[{\"path_line\":\"a.py:1\",\"symbol\":\"f\",\"evidence\":\"source\",\"confidence\":\"high\"}],\"line_coverage_pct\":91.2}\n```", "usage": map[string]any{"total_tokens": 21}}
			if failed {
				result = map[string]any{"state": "FAILED", "errorCode": "CODEX_FAILED", "message": "worker failed"}
			}
			fake := &fakeJobs{jobID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", results: []map[string]any{result}}
			runner := Runner{Jobs: fake, Sleep: func(_ time.Duration) {}}
			path, err := RunAndPersistReport(runner, state, repo, "test-gaps")
			key := filepath.Join(state, "projects", contextProjectKey(repo), "maintenance", "reports", "test-gaps")
			if failed {
				if err == nil || path != "" {
					t.Fatalf("path=%q error=%v", path, err)
				}
				matches, _ := filepath.Glob(filepath.Join(key, "failures", "*.json"))
				if len(matches) != 1 {
					t.Fatalf("failure files: %v", matches)
				}
				var record map[string]any
				data, _ := os.ReadFile(matches[0])
				if json.Unmarshal(data, &record) != nil || record["state"] != "FAILED" || record["job_id"] != fake.jobID {
					t.Fatalf("failure record: %s", data)
				}
				reports, _ := filepath.Glob(filepath.Join(key, "*.md"))
				if len(reports) != 0 {
					t.Fatalf("failure created report: %v", reports)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			prompt, _ := BuildReportPrompt(repo, "test-gaps")
			if fake.started["prompt"] != prompt {
				t.Fatal("runner prompt differs from builder")
			}
			content, _ := os.ReadFile(path)
			if !strings.Contains(string(content), "- Findings: 1") || !strings.Contains(string(content), `"total_tokens":21`) {
				t.Fatalf("report content: %s", content)
			}
			lastRun := filepath.Join(state, "projects", contextProjectKey(repo), "maintenance", "reports", "test-gaps", "last-run.json")
			if _, err := os.Stat(lastRun); err != nil {
				t.Fatal(err)
			}
			notice, _ := os.ReadFile(strings.TrimSuffix(path, ".md") + ".notice.json")
			if !strings.Contains(string(notice), `"line_coverage_pct":91.2`) {
				t.Fatalf("notice: %s", notice)
			}
			events, _ := filepath.Glob(filepath.Join(state, "telemetry", "events", "*", "session_unknown.jsonl"))
			if len(events) != 1 {
				t.Fatalf("completion telemetry files: %v", events)
			}
			lines, _ := os.ReadFile(events[0])
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(string(lines))), &event) != nil || event["event_type"] != "MAINTENANCE_REPORT_FINISHED" {
				t.Fatalf("completion telemetry: %s", lines)
			}
			payload := event["payload"].(map[string]any)
			if payload["findings_high_count"] != float64(1) || payload["line_coverage_pct"] != 91.2 || payload["quota_change_active_window_pct"] != nil {
				t.Fatalf("completion telemetry payload: %#v", payload)
			}
		})
	}
}

func contextProjectKey(repository string) string {
	return contextio.ProjectKey(repository)
}

func maintenanceScratch(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", ".tmp", "maintenance-report-tests")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := os.MkdirTemp(root, "case-")
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}
