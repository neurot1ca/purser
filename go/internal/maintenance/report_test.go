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
	cleanup, err := BuildReportPrompt(repo, "cleanup", 90)
	if err != nil {
		t.Fatal(err)
	}
	wantCleanup := "Produce a cleanup report for the repository at <REPO>.\n" +
		"This is a strictly read-only inspection. Do not edit files, create repository files,\n" +
		"install tools, or run package managers. Detect languages from `git ls-files -co --exclude-standard`\n" +
		"extensions, go.mod, package.json, Cargo.toml, pyproject.toml/setup.cfg, and shebangs.\n" +
		"Use only locally resolved, already-installed tools; never\n" +
		"download tools, dependencies, or toolchains (including npx fetches, `go run\n" +
		"pkg@version`, or `cargo install`). Report only evidence-backed candidates.\n" +
		"For Go use deadcode, else staticcheck -checks U1000; TS/JS use project-local knip\n" +
		"or ts-prune; Rust use compiler dead_code warnings (`cargo check --offline` only if\n" +
		"it needs no download); Python use vulture. Other languages use an existing configured\n" +
		"analyzer. If none is usable, use search evidence and state its limits for that language.\n" +
		"Include a compact per-language scope table: language, file count, tool+version or\n" +
		"search-only/not-scanned, command, and limitation. For each item include exact tool\n" +
		"output or search evidence, repository path and symbol, and checks against entry points,\n" +
		"hooks, and dynamic/reflection use before calling it unused. Include commands/searches\n" +
		"and distinguish uncertain candidates from confirmed facts.\n" +
		"If user-initiated work starts, it has priority: do not delay it, cancel it, or\n" +
		"continue consuming resources once it needs the machine. End your report with this\n" +
		"machine-readable list, using one object per finding and an empty list for no findings:\n" +
		"```purser-findings-json\n" +
		"[{\"path_line\":\"relative/path.py:12\",\"symbol\":\"name\",\"evidence\":\"short evidence\",\"confidence\":\"high|medium|low\"}]\n```\n"
	if cleanup != wantCleanup {
		t.Fatalf("cleanup prompt mismatch\n got: %q\nwant: %q", cleanup, wantCleanup)
	}
	testGaps, err := BuildReportPrompt(repo, "test-gaps", 90)
	if err != nil {
		t.Fatal(err)
	}
	wantTestGaps := "Produce a test-gaps report for the repository at <REPO>.\n" +
		"This is a strictly read-only inspection. Do not edit files, create repository files,\n" +
		"install tools, or run package managers. Detect languages from `git ls-files -co --exclude-standard`\n" +
		"extensions, go.mod, package.json, Cargo.toml, pyproject.toml/setup.cfg, and shebangs.\n" +
		"Use only locally resolved, already-installed tools; never\n" +
		"download tools, dependencies, toolchains, or coverage providers (including npx\n" +
		"fetches, `go run pkg@version`, or `cargo install`). Report only evidence-bound tests.\n" +
		"For each candidate, give the target path:line and symbol, the untested behaviour,\n" +
		"evidence from source, the exact command used to search existing tests and its result\n" +
		"showing no test already covers that behaviour, and a high, medium, or low confidence\n" +
		"level. Include a compact contract evidence section covering documented requirements,\n" +
		"invariants, boundaries, and affected callers, cite each item to a source path:line,\n" +
		"and write \"unknown\" where evidence is absent.\n" +
		"Measure each language with existing local tools/data: Go existing coverprofile or\n" +
		"`go test -cover` (statement coverage); TS/JS configured jest, vitest, or c8 coverage;\n" +
		"Rust installed cargo llvm-cov or tarpaulin; Python existing data via `python -m\n" +
		"coverage report`; others an existing configured command/artifact. Never fetch a tool,\n" +
		"provider, or dependency. If unavailable, use search evidence and state its limits.\n" +
		"Include a compact per-language scope table: language, file count, tool+version or search-only/\n" +
		"not-scanned, command, limitation. State coverage per language and overall against a\n" +
		"90% line-coverage target; order candidate tests by how much uncovered code they\n" +
		"exercise. line_coverage_pct is the overall number or null if any detected language\n" +
		"lacks a measurement.\n" +
		"If user-initiated work starts, it has priority: do not delay it, cancel it, or\n" +
		"continue consuming resources once it needs the machine. End with one JSON object in\n" +
		"this machine-readable block. Findings use the cleanup shape; line_coverage_pct must\n" +
		"be the overall percentage or null:\n```purser-findings-json\n" +
		"{\"findings\":[{\"path_line\":\"relative/path.py:12\",\"symbol\":\"name\",\"evidence\":\"source evidence; untested behaviour; test search command and no-match result\",\"confidence\":\"high|medium|low\"}],\"line_coverage_pct\":null}\n```\n"
	if testGaps != wantTestGaps {
		t.Fatalf("test-gaps prompt mismatch\n got: %q\nwant: %q", testGaps, wantTestGaps)
	}
}

func TestReportPromptsRequireNoDownloadsAndLanguageScope(t *testing.T) {
	for _, reportType := range []string{"cleanup", "test-gaps"} {
		prompt, err := BuildReportPrompt("<REPO>", reportType, 90)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(prompt, "never\ndownload tools, dependencies") {
			t.Errorf("%s prompt lacks the no-download rule", reportType)
		}
		if !strings.Contains(prompt, "per-language scope table") {
			t.Errorf("%s prompt lacks the language scope table instruction", reportType)
		}
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
	body := "```purser-findings-json\n{\"findings\":[{\"path_line\":\"a.py:1\",\"symbol\":\"a\",\"evidence\":\"e\",\"confidence\":\"low\"},{\"path_line\":\"b.py:2\",\"symbol\":\"b\",\"evidence\":\"e\",\"confidence\":\"high\"}],\"line_coverage_pct\":80}\n```"
	if got := ParseFindings(body, "test-gaps"); got[0]["symbol"] != "a" {
		t.Fatalf("test-gaps worker order = %#v", got)
	}
	if got := ParseFindings(body, "cleanup"); got[0]["symbol"] != "b" {
		t.Fatalf("cleanup confidence order = %#v", got)
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
			if !failed {
				profileDir := filepath.Join(repo, ".orchestrator")
				if err := os.MkdirAll(profileDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(profileDir, "profile.toml"), []byte("schema_version = 1\n[maintenance]\ncoverage_target_pct = 80\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result := map[string]any{"state": "SUCCEEDED", "content": "Body.\n```purser-findings-json\n{\"findings\":[{\"path_line\":\"a.py:1\",\"symbol\":\"f\",\"evidence\":\"source\",\"confidence\":\"low\"}],\"line_coverage_pct\":80}\n```", "usage": map[string]any{"total_tokens": 21}}
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
			prompt, _ := BuildReportPrompt(repo, "test-gaps", 80)
			if fake.started["prompt"] != prompt {
				t.Fatal("runner prompt differs from builder")
			}
			if !strings.Contains(prompt, "80% line-coverage target") {
				t.Fatalf("override missing from prompt: %s", prompt)
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
			if !strings.Contains(string(notice), `"line_coverage_pct":80`) || !strings.Contains(string(notice), `"coverage_target_pct":80`) {
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
			if payload["findings_high_count"] != float64(0) || payload["line_coverage_pct"] != float64(80) || payload["quota_change_active_window_pct"] != nil {
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
