package optimize

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func optimizeTool(name string, input map[string]any, id string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func optimizeUser(text, entrypoint string, extra map[string]any) map[string]any {
	record := map[string]any{
		"type": "user", "entrypoint": entrypoint, "userType": "external",
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}},
	}
	for key, value := range extra {
		record[key] = value
	}
	return record
}

func optimizeAssistant(key, entrypoint string, weight float64, text string, tools []map[string]any) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tool := range tools {
		content = append(content, tool)
	}
	return map[string]any{
		"type": "assistant", "entrypoint": entrypoint, "requestId": key,
		"message": map[string]any{
			"id": key, "role": "assistant", "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": weight}, "content": content,
		},
	}
}

func optimizeSkillListing(rows ...[2]string) map[string]any {
	names := make([]any, 0, len(rows))
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row[0])
		lines = append(lines, fmt.Sprintf("- %s: %s", row[0], row[1]))
	}
	return map[string]any{
		"type": "attachment", "entrypoint": "cli",
		"attachment": map[string]any{
			"type": "skill_listing", "content": strings.Join(lines, "\n"), "names": names,
			"skillCount": len(names), "isInitial": true,
		},
	}
}

func optimizeMCPListing(server, description string) map[string]any {
	if description == "" {
		description = "instructions for a server"
	}
	return map[string]any{
		"type": "attachment", "entrypoint": "cli",
		"attachment": map[string]any{
			"type": "mcp_instructions_delta", "addedNames": []any{server},
			"addedBlocks": []any{fmt.Sprintf("## %s\n%s", server, description)},
		},
	}
}

func writeOptimizeSession(t *testing.T, root, name string, records []any, ageDays int) string {
	t.Helper()
	path := filepath.Join(root, "project", name+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, record := range records {
		if raw, ok := record.(string); ok {
			lines = append(lines, raw)
			continue
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return path
}

func copyOptimizeFixture(t *testing.T, fixture, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "project", name+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func optimizeReport(t *testing.T, projects, home, repo string, days int) map[string]any {
	t.Helper()
	report, err := Analyze(projects, home, repo, days)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func optimizeStartup(report map[string]any) []startupItem {
	return report["startup"].([]startupItem)
}

func optimizeHidden(report map[string]any) []hiddenRow {
	return report["hidden"].([]hiddenRow)
}

func optimizeRow(items []startupItem, name string) startupItem {
	for _, item := range items {
		if item.name == name {
			return item
		}
	}
	return startupItem{}
}

func optimizeSequenceSession(t *testing.T, projects, name string) {
	t.Helper()
	commands := []string{"git add -A", "git commit -m update", "claude plugin list"}
	records := []any{}
	for turn := 0; turn < 3; turn++ {
		records = append(records, optimizeUser(fmt.Sprintf("turn %d", turn), "cli", nil))
		for step, command := range commands {
			records = append(records, optimizeAssistant(fmt.Sprintf("%s-%d-%d", name, turn, step), "cli", 10, "ok", []map[string]any{
				optimizeTool("Bash", map[string]any{"command": command}, fmt.Sprintf("%s-tool-%d-%d", name, turn, step)),
			}))
		}
	}
	writeOptimizeSession(t, projects, name, records, 0)
}

func optimizeRenderReport(startup []startupItem, hidden []hiddenRow, sequences []sequence, days int) string {
	return render(map[string]any{
		"startup": startup, "hidden": hidden, "sequences": sequences, "days": days,
		"compaction":       map[string]any{"enabled": true, "threshold_tokens": 250000},
		"duplicate_skills": []map[string]any{}, "median_reply_chars": float64(0),
		"reply_count": 0, "reply_rule_present": false,
	})
}

func TestOptimizeUnusedPluginAndUsedSkill(t *testing.T) {
	root := t.TempDir()
	projects, home := filepath.Join(root, "projects"), filepath.Join(root, "home")
	listing := optimizeSkillListing([2]string{"unused-plugin:unused", strings.Repeat("x", 2400)}, [2]string{"used-plugin:used", "short"}, [2]string{"small", "z"})
	records := []any{listing, optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", []map[string]any{optimizeTool("Skill", map[string]any{"skill": "used"}, "tool1")})}
	writeOptimizeSession(t, projects, "s", records, 0)
	report := optimizeReport(t, projects, home, "", 30)
	rows := map[string]startupItem{}
	for _, row := range optimizeStartup(report) {
		rows[row.name] = row
	}
	if !rows["plugin:unused-plugin"].flagged || rows["skill:used-plugin:used"].flagged || rows["skill:small"].flagged {
		t.Fatalf("startup flags = %#v", rows)
	}
}

func TestOptimizeWeightedTotalsDedupeMessageID(t *testing.T) {
	root := t.TempDir()
	copyOptimizeFixture(t, "weighted_totals.jsonl", filepath.Join(root, "projects"), "s")
	report := optimizeReport(t, filepath.Join(root, "projects"), root, "", 30)
	if report["weighted_tokens"] != float64(58) || report["requests"] != 1 {
		t.Fatalf("weighted report = %#v", report)
	}
}

func TestOptimizeHiddenEntrypointPromptSignatureAndShare(t *testing.T) {
	root := t.TempDir()
	projects, home := filepath.Join(root, "projects"), filepath.Join(root, "home")
	writeOptimizeSession(t, projects, "sdk", []any{optimizeUser("Run security review for sdk-py", "sdk-py", map[string]any{"promptSource": "sdk"}), optimizeAssistant("a", "sdk-py", 300, "ok", nil)}, 0)
	report := optimizeReport(t, projects, home, "", 30)
	hidden := optimizeHidden(report)
	if len(hidden) != 1 || hidden[0].entrypoint != "sdk-py" || hidden[0].share != 1 || hidden[0].weightedTokens != 300 {
		t.Fatalf("hidden report = %#v", hidden)
	}
}

func TestOptimizeDuplicateSkillSources(t *testing.T) {
	t.Run("project and plugin", func(t *testing.T) {
		root := t.TempDir()
		project := filepath.Join(root, "workspace")
		if err := os.MkdirAll(filepath.Join(project, ".claude", "skills", "x"), 0700); err != nil {
			t.Fatal(err)
		}
		listing := optimizeSkillListing([2]string{"x", "local"}, [2]string{"plugin:x", "plugin copy"})
		writeOptimizeSession(t, filepath.Join(root, "projects"), "s", []any{listing, optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", nil)}, 0)
		report := optimizeReport(t, filepath.Join(root, "projects"), root, project, 30)
		if !hasDuplicateSkill(report, "x", []string{"plugin", "project"}) {
			t.Fatalf("duplicate skills = %#v", report["duplicate_skills"])
		}
	})
	t.Run("user and plugin", func(t *testing.T) {
		root := t.TempDir()
		home := filepath.Join(root, "home")
		if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "review"), 0700); err != nil {
			t.Fatal(err)
		}
		writeOptimizeSession(t, filepath.Join(root, "projects"), "s", []any{optimizeSkillListing([2]string{"plugin:review", "plugin skill"}), optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", nil)}, 0)
		report := optimizeReport(t, filepath.Join(root, "projects"), home, "", 30)
		if !hasDuplicateSkill(report, "review", []string{"plugin", "user"}) {
			t.Fatalf("duplicate skills = %#v", report["duplicate_skills"])
		}
	})
}

func hasDuplicateSkill(report map[string]any, name string, sources []string) bool {
	want := append([]string(nil), sources...)
	sort.Strings(want)
	for _, item := range report["duplicate_skills"].([]map[string]any) {
		gotName, _ := item["name"].(string)
		got, _ := item["sources"].([]string)
		sort.Strings(got)
		if gotName == name && reflect.DeepEqual(got, want) {
			return true
		}
	}
	return false
}

func TestOptimizeReplyRuleBoundary(t *testing.T) {
	for _, test := range []struct {
		length int
		want   bool
	}{
		{800, false}, {801, true},
	} {
		t.Run(fmt.Sprint(test.length), func(t *testing.T) {
			root := t.TempDir()
			projects, home := filepath.Join(root, "projects"), filepath.Join(root, "home")
			writeOptimizeSession(t, projects, "s", []any{optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, strings.Repeat("x", test.length), nil)}, 0)
			report := optimizeReport(t, projects, home, "", 30)
			median, _ := report["median_reply_chars"].(float64)
			if (median > 800) != test.want {
				t.Fatalf("median = %v", median)
			}
			output := render(report)
			if strings.Contains(output, "Your replies average ~801 characters.") != test.want {
				t.Fatalf("reply output = %s", output)
			}
			if strings.Contains(output, "Add reply rule to CLAUDE.md") != test.want {
				t.Fatalf("reply action output = %s", output)
			}
		})
	}
}

func TestOptimizeReplyHeadingAlreadyPresent(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("# Replies\nBe concise.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeOptimizeSession(t, filepath.Join(root, "projects"), "s", []any{optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, strings.Repeat("x", 900), nil)}, 0)
	output := render(optimizeReport(t, filepath.Join(root, "projects"), home, "", 30))
	if strings.Contains(output, "Your replies average") || !strings.Contains(output, "Reply rule: # Replies is present") {
		t.Fatalf("reply heading output = %s", output)
	}
}

func TestOptimizeRepeatedSequenceRequiresThreeSessions(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	for index := 0; index < 3; index++ {
		optimizeSequenceSession(t, projects, fmt.Sprintf("session%d", index))
	}
	report := optimizeReport(t, projects, root, "", 30)
	sequences := report["sequences"].([]sequence)
	if len(sequences) == 0 || len(sequences[0].steps) != 3 || sequences[0].sessions != 3 {
		t.Fatalf("sequences = %#v", sequences)
	}
	onlyTwo := filepath.Join(root, "two-projects")
	for index := 0; index < 2; index++ {
		optimizeSequenceSession(t, onlyTwo, fmt.Sprintf("session%d", index))
	}
	if got := optimizeReport(t, onlyTwo, root, "", 30)["sequences"].([]sequence); len(got) != 0 {
		t.Fatalf("two-session sequences = %#v", got)
	}
}

func TestOptimizeOldFilesIgnoredAndMalformedLinesSkipped(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	writeOptimizeSession(t, projects, "old", []any{optimizeUser("hello", "cli", nil), optimizeAssistant("old", "cli", 99, "ok", nil)}, 90)
	writeOptimizeSession(t, projects, "fresh", []any{"not json", optimizeUser("hello", "cli", nil), optimizeAssistant("fresh", "cli", 7, "ok", nil)}, 0)
	report := optimizeReport(t, projects, root, "", 30)
	if report["sessions"] != 1 || report["weighted_tokens"] != float64(7) {
		t.Fatalf("report = %#v", report)
	}
}

func TestOptimizeNoFilesWrittenAndCompactionSetting(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	config := filepath.Join(home, ".orchestrator", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("[compaction]\nenabled = false\nthreshold_tokens = 123456\n"), 0600); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(root, "projects")
	writeOptimizeSession(t, projects, "s", []any{optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", nil)}, 0)
	before := optimizeFiles(t, root)
	report := optimizeReport(t, projects, home, "", 30)
	after := optimizeFiles(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("files changed: before=%v after=%v", before, after)
	}
	if !reflect.DeepEqual(report["compaction"], map[string]any{"enabled": false, "threshold_tokens": 123456}) || !strings.Contains(render(report), "Compaction advice: Off; threshold: 123456") {
		t.Fatalf("compaction report = %#v", report)
	}
}

func optimizeFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func TestOptimizeMCPUseAndDeferredNamesContributeStartupCost(t *testing.T) {
	root := t.TempDir()
	deferred := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "deferred_tools_delta", "addedNames": []any{"mcp__search_server__query"}}}
	records := []any{optimizeMCPListing("search-server", strings.Repeat("instructions ", 100)), deferred, optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", []map[string]any{optimizeTool("mcp__search_server__query", nil, "tool1")})}
	writeOptimizeSession(t, filepath.Join(root, "projects"), "s", records, 0)
	report := optimizeReport(t, filepath.Join(root, "projects"), root, "", 30)
	startup := optimizeStartup(report)
	row := optimizeRow(startup, "mcp:search-server")
	count := 0
	for _, item := range startup {
		if item.kind == "MCP" {
			count++
		}
	}
	if count != 1 || row.uses != 0 || row.tokensPerRequest < 100 || !row.flagged {
		t.Fatalf("MCP startup = %#v", startup)
	}
}

func TestOptimizeLatestMCPInventoryReplaysRemovals(t *testing.T) {
	root := t.TempDir()
	deferred := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "deferred_tools_delta", "addedNames": []any{"mcp__x__call", "mcp__still_enabled__query"}}}
	removeInstructions := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "mcp_instructions_delta", "removedNames": []any{"x"}}}
	removeDeferred := map[string]any{"type": "attachment", "attachment": map[string]any{"type": "deferred_tools_delta", "removedNames": []any{"mcp__x__call"}}}
	records := []any{optimizeMCPListing("x", ""), optimizeMCPListing("still_enabled", ""), deferred, removeInstructions, removeDeferred, optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", nil)}
	writeOptimizeSession(t, filepath.Join(root, "projects"), "s", records, 0)
	report := optimizeReport(t, filepath.Join(root, "projects"), root, "", 30)
	names := map[string]bool{}
	for _, item := range optimizeStartup(report) {
		names[item.name] = true
	}
	if names["mcp:x"] || !names["mcp:still_enabled"] {
		t.Fatalf("MCP names = %#v", names)
	}
}

func TestOptimizeCompactionDefaultsOn(t *testing.T) {
	root := t.TempDir()
	report := optimizeReport(t, filepath.Join(root, "projects"), filepath.Join(root, "home"), "", 30)
	compaction := report["compaction"].(map[string]any)
	if compaction["enabled"] != true || compaction["threshold_tokens"] != 250000 || !strings.Contains(render(report), "Compaction advice: On; threshold: 250000") {
		t.Fatalf("compaction = %#v", compaction)
	}
}

func TestOptimizeLatestInteractiveStartupIsDedupedButUsageSpansWindow(t *testing.T) {
	root := t.TempDir()
	projects, workspace := filepath.Join(root, "projects"), filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, ".claude", "skills", "local"), 0700); err != nil {
		t.Fatal(err)
	}
	oldListing := optimizeSkillListing([2]string{"stale:thing", "old startup"})
	old := []any{oldListing, optimizeUser("hello", "cli", nil), optimizeAssistant("old-use", "cli", 100, "ok", []map[string]any{optimizeTool("Skill", map[string]any{"skill": "local"}, "old-skill-call")})}
	writeOptimizeSession(t, projects, "older", old, 2)
	currentListing := optimizeSkillListing([2]string{"local", "local copy"}, [2]string{"current:local", "plugin copy"})
	current := []any{currentListing, currentListing, optimizeUser("hello", "cli", nil), optimizeAssistant("current-1", "cli", 100, "ok", nil), optimizeUser("next", "cli", nil), optimizeAssistant("current-2", "cli", 100, "ok", nil)}
	writeOptimizeSession(t, projects, "current", current, 0)
	report := optimizeReport(t, projects, root, workspace, 30)
	rows := map[string]startupItem{}
	for _, item := range optimizeStartup(report) {
		rows[item.name] = item
	}
	if _, stale := rows["skill:stale:thing"]; stale || rows["skill:local"].requestsLoaded != 2 || rows["skill:local"].uses != 1 || !hasDuplicateSkill(report, "local", []string{"current", "project"}) {
		t.Fatalf("startup or duplicate report = %#v / %#v", rows, report["duplicate_skills"])
	}
	for _, item := range report["duplicate_skills"].([]map[string]any) {
		if item["name"] == "thing" {
			t.Fatalf("stale duplicate skill = %#v", item)
		}
	}
}

func TestOptimizeNoninitialSkillListingAddsToInventory(t *testing.T) {
	root := t.TempDir()
	initial := optimizeSkillListing([2]string{"a", "initial skill"}, [2]string{"plugin:b", "plugin b"})
	update := optimizeSkillListing([2]string{"plugin:c", "plugin c"})
	update["attachment"].(map[string]any)["isInitial"] = false
	writeOptimizeSession(t, filepath.Join(root, "projects"), "s", []any{initial, update, optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", nil)}, 0)
	report := optimizeReport(t, filepath.Join(root, "projects"), root, "", 30)
	names := map[string]bool{}
	for _, item := range optimizeStartup(report) {
		names[item.name] = true
	}
	for _, name := range []string{"skill:a", "skill:plugin:b", "skill:plugin:c"} {
		if !names[name] {
			t.Fatalf("missing %s in %#v", name, names)
		}
	}
}

func TestOptimizeBuiltinUnprefixedSkillsAreNotFlaggedOrDuplicated(t *testing.T) {
	root := t.TempDir()
	listing := optimizeSkillListing([2]string{"code-review", strings.Repeat("x", 2400)}, [2]string{"plugin:code-review", "plugin copy"})
	writeOptimizeSession(t, filepath.Join(root, "projects"), "s", []any{listing, optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", nil)}, 0)
	report := optimizeReport(t, filepath.Join(root, "projects"), root, filepath.Join(root, "workspace"), 30)
	row := optimizeRow(optimizeStartup(report), "skill:code-review")
	for _, item := range report["duplicate_skills"].([]map[string]any) {
		if item["name"] == "code-review" {
			t.Fatalf("builtin duplicate = %#v", item)
		}
	}
	if !row.builtIn || row.flagged {
		t.Fatalf("builtin row = %#v", row)
	}
}

func TestOptimizeShellSignaturesStripNavigationPrefixes(t *testing.T) {
	bash := tool{name: "Bash", input: map[string]any{"command": "cd /tmp && cd 'C:/work tree'; python scripts/release.py --check"}}
	powershell := tool{name: "PowerShell", input: map[string]any{"command": "Set-Location C:/one; pushd C:/two && git status --short"}}
	if got := signature([]tool{bash}); !reflect.DeepEqual(got, sequenceStep{"Bash python scripts/release.py"}) {
		t.Fatalf("bash signature = %#v", got)
	}
	if got := signature([]tool{powershell}); !reflect.DeepEqual(got, sequenceStep{"PowerShell git status"}) {
		t.Fatalf("PowerShell signature = %#v", got)
	}
}

func optimizeMockSequenceSession(name string, commands []string) session {
	requests := make([]request, 0, len(commands))
	for index, command := range commands {
		requests = append(requests, request{weight: 10, tools: []tool{{name: "Bash", id: fmt.Sprintf("%s-%d", name, index), input: map[string]any{"command": command}}}})
	}
	return session{path: name, requests: requests, requestTurns: [][]int{makeRange(len(requests))}}
}

func makeRange(length int) []int {
	values := make([]int, length)
	for index := range values {
		values[index] = index
	}
	return values
}

func TestOptimizeRepeatedNgramsNeedTwoSignaturesAndCountNonOverlapping(t *testing.T) {
	identical := []session{}
	for index := 0; index < 3; index++ {
		identical = append(identical, optimizeMockSequenceSession(fmt.Sprintf("same-%d", index), []string{"git add file", "git add file", "git add file", "git add file", "git add file", "git add file"}))
	}
	if got := sequenceCandidates(identical); len(got) != 0 {
		t.Fatalf("identical candidates = %#v", got)
	}
	mixed := []session{}
	for index := 0; index < 3; index++ {
		mixed = append(mixed, optimizeMockSequenceSession(fmt.Sprintf("mixed-%d", index), []string{"git add file", "python run.py", "git add file", "python run.py", "git add file", "python run.py"}))
	}
	candidates := sequenceCandidates(mixed)
	aba := sequence{steps: []sequenceStep{{"Bash git add"}, {"Bash python run.py"}, {"Bash git add"}}}
	bab := sequence{steps: []sequenceStep{{"Bash python run.py"}, {"Bash git add"}, {"Bash python run.py"}}}
	if got := sequenceOccurrences(candidates, aba.steps); got != 3 {
		t.Fatalf("ABA occurrences = %d, candidates = %#v", got, candidates)
	}
	if got := sequenceOccurrences(candidates, bab.steps); got != 3 {
		t.Fatalf("BAB occurrences = %d, candidates = %#v", got, candidates)
	}
}

func sequenceOccurrences(candidates []sequence, steps []sequenceStep) int {
	for _, candidate := range candidates {
		if reflect.DeepEqual(candidate.steps, steps) {
			return candidate.occurrences
		}
	}
	return 0
}

func TestOptimizeSequencesRequireScriptableRequests(t *testing.T) {
	sessions := []session{}
	inlineCommands := []string{"python -", "python -c print('x')", "python3 -", "python3 -c print('x')", "py -", "py -c print('x')", "node -e console.log('x')", "powershell -Command Write-Output x", "pwsh -c Write-Output x", "python <<'PY'", "python - <<'PY'"}
	for index := 0; index < 3; index++ {
		requests := []request{}
		readEdit := []tool{{name: "Read"}, {name: "Edit"}, {name: "Edit"}}
		grepSed := []tool{{name: "Bash", input: map[string]any{"command": "grep -n pattern file"}}, {name: "Bash", input: map[string]any{"command": "sed -n 1,3p file"}}, {name: "Bash", input: map[string]any{"command": "sed -n 4,6p file"}}}
		for _, item := range readEdit {
			if eligible([]tool{item}) {
				t.Fatalf("non-scriptable tool became eligible: %#v", item)
			}
			requests = append(requests, request{weight: 10, tools: []tool{item}})
		}
		for _, item := range grepSed {
			if eligible([]tool{item}) {
				t.Fatalf("read-only command became eligible: %#v", item)
			}
			requests = append(requests, request{weight: 10, tools: []tool{item}})
		}
		for _, command := range inlineCommands {
			item := tool{name: "Bash", input: map[string]any{"command": command}}
			if eligible([]tool{item}) {
				t.Fatalf("inline command became eligible: %s", command)
			}
			requests = append(requests, request{weight: 10, tools: []tool{item}})
		}
		for _, command := range []string{"git add -A", "git commit -m message", "claude plugin marketplace list"} {
			requests = append(requests, request{weight: 10, tools: []tool{{name: "Bash", input: map[string]any{"command": command}}}})
		}
		sessions = append(sessions, session{path: fmt.Sprintf("eligible-%d", index), requests: requests, requestTurns: [][]int{makeRange(len(requests))}})
	}
	candidates := sequenceCandidates(sessions)
	positive := []sequenceStep{{"Bash git add"}, {"Bash git commit"}, {"Bash claude plugin"}}
	if candidate := sequenceOccurrences(candidates, positive); candidate != 3 {
		t.Fatalf("positive sequence occurrences = %d, candidates = %#v", candidate, candidates)
	}
	for _, candidate := range candidates {
		joined := strings.ToLower(fmt.Sprint(candidate.steps))
		for _, marker := range []string{"read", "edit", "grep", "sed", "python -", "python3 -", "py -", "node -e", "powershell -command", "pwsh -c"} {
			if strings.Contains(joined, marker) {
				t.Fatalf("non-scriptable marker %q in %#v", marker, candidate)
			}
		}
	}
}

func TestOptimizeContainedSequenceAndLargeValues(t *testing.T) {
	short := []sequenceStep{{"A"}, {"B"}, {"A"}}
	long := []sequenceStep{{"A"}, {"B"}, {"A"}, {"C"}}
	candidates := []sequence{{steps: short, saving: 100}, {steps: long, saving: 90}}
	if got := selectSequences(candidates, 5); len(got) != 1 || !reflect.DeepEqual(got[0].steps, short) {
		t.Fatalf("selected sequences = %#v", got)
	}
	if got := formatNumber(18_022_000); got != "18.0M" {
		t.Fatalf("formatNumber = %s", got)
	}
	report := optimizeRenderReport(nil, nil, []sequence{{steps: short, occurrences: 3, sessions: 3, saving: 18_022_000}}, 30)
	if !strings.Contains(report, "~18M / 30 days") || !strings.Contains(report, "> ⚓ **Purser optimize**") {
		t.Fatalf("sequence output = %s", report)
	}
}

func TestOptimizeRenderCostsScaleTo500Messages(t *testing.T) {
	output := optimizeRenderReport([]startupItem{{name: "plugin:design-tool", kind: "plugin", tokensPerRequest: 530.6, requestsLoaded: 14, flagged: true}}, nil, nil, 30)
	if !strings.Contains(output, "~265.3K / 500 msg") || !strings.Contains(output, "Disable plugin") {
		t.Fatalf("plugin render = %s", output)
	}
}

func TestOptimizeRenderRepeatedSequenceKeepsWindowTotal(t *testing.T) {
	output := optimizeRenderReport(nil, nil, []sequence{{steps: []sequenceStep{{"Bash git add"}, {"Bash git commit"}}, saving: 480_000, occurrences: 4, sessions: 3}}, 30)
	if !strings.Contains(output, "~480K / 30 days") || strings.Contains(output, "/ 500 msg") || !strings.Contains(output, "Make one script") {
		t.Fatalf("sequence render = %s", output)
	}
}

func TestOptimizeRenderHiddenSessionsUseAbsoluteCountAndUnknownStatus(t *testing.T) {
	output := optimizeRenderReport(nil, []hiddenRow{{entrypoint: "sdk-py", sessions: 174, weightedTokens: 21_000_000, share: .04}}, nil, 30)
	if !strings.Contains(output, "unknown launcher: 174 hidden sessions") || !strings.Contains(output, "~21M / 30 days") || !strings.Contains(output, "status unknown") || strings.Contains(output, "weighted") || strings.Contains(output, "%") {
		t.Fatalf("hidden render = %s", output)
	}
	if !strings.HasSuffix(output, "\n> \n> *🧠 Purser learns as it works in this project: it measures how long its work really takes, sharpens its estimates, and points out where tokens and time can be saved.*") {
		t.Fatalf("hidden footer = %s", output)
	}
}

func TestOptimizeRenderHiddenPromptIsNotRenderedPastLimit(t *testing.T) {
	prompt := "Investigate this unusually long security issue involving a vulnerable dependency across several repositories"
	output := optimizeRenderReport(nil, []hiddenRow{{entrypoint: "sdk-py", sessions: 1, weightedTokens: 500}}, nil, 30)
	if !strings.Contains(output, "unknown launcher") || strings.Contains(output, prompt[:60]+"…") {
		t.Fatalf("hidden prompt render = %s", output)
	}
}

func TestOptimizeRenderRawMCPUUIDIsNeverRendered(t *testing.T) {
	connectorID := "1a59c906-04da-521d-bda7-7f71b9f9e01c"
	output := optimizeRenderReport([]startupItem{{name: "mcp:" + connectorID, kind: "MCP", tokensPerRequest: 100, requestsLoaded: 14, flagged: true}}, nil, nil, 30)
	if strings.Contains(output, connectorID) || !strings.Contains(output, "unnamed claude.ai connector") {
		t.Fatalf("UUID render = %s", output)
	}
}

func TestOptimizeRenderAnthropicDocumentSkillsAreGrouped(t *testing.T) {
	startup := []startupItem{}
	for _, name := range []string{"pptx", "xlsx", "docx", "pdf"} {
		startup = append(startup, startupItem{name: "skill:anthropic:document-skills:" + name, kind: "skill", tokensPerRequest: 10, totalTokens: 100, flagged: true})
	}
	output := optimizeRenderReport(startup, nil, nil, 30)
	if !strings.Contains(output, "Unused document skills (pptx, xlsx, docx, pdf)") || strings.Count(output, "Remove skills") != 1 {
		t.Fatalf("document skill render = %s", output)
	}
}

func TestOptimizeRenderDiagnosticsFollowRankedTable(t *testing.T) {
	output := optimizeRenderReport([]startupItem{{name: "plugin:design", kind: "plugin", tokensPerRequest: 100, requestsLoaded: 2, flagged: true}}, nil, nil, 30)
	if strings.Index(output, "| 1 |") >= strings.Index(output, "No additional duplicate-skill issues") || !strings.Contains(output, "System prompts and eager tool schemas are not recorded") || !strings.Contains(output, "Compaction advice: On") || !strings.Contains(output, "Median final reply") {
		t.Fatalf("diagnostic render = %s", output)
	}
}

func TestOptimizeDisabledPluginIsStoppedWithNoAction(t *testing.T) {
	root := t.TempDir()
	home, projects, project := filepath.Join(root, "home"), filepath.Join(root, "projects"), filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"enabledPlugins":{"security-guidance@official":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	assistant := optimizeAssistant("a", "cli", 100, "ok", nil)
	assistant["cwd"] = project
	records := []any{optimizeSkillListing([2]string{"security-guidance:unused", strings.Repeat("x", 2400)}), optimizeUser("hello", "cli", map[string]any{"cwd": project}), assistant}
	writeOptimizeSession(t, projects, "s", records, 5)
	writeOptimizeSession(t, projects, "latest", []any{optimizeUser("background job", "sdk-py", map[string]any{"promptSource": "sdk"}), optimizeAssistant("background", "sdk-py", 100, "ok", nil)}, 0)
	report := optimizeReport(t, projects, home, "", 30)
	row := optimizeRow(optimizeStartup(report), "plugin:security-guidance")
	output := render(report)
	if row.status != "stopped" || row.lastSeen != "" || !strings.Contains(output, "stopped: plugin disabled") || strings.Contains(output, "Disable plugin") {
		t.Fatalf("disabled plugin report = %#v\n%s", row, output)
	}
}

func TestOptimizeHiddenPromptMatchesInstalledPlugin(t *testing.T) {
	root := t.TempDir()
	home, projects := filepath.Join(root, "home"), filepath.Join(root, "projects")
	install := filepath.Join(home, ".claude", "plugins", "cache", "official", "security-guidance", "1.0")
	if err := os.MkdirAll(install, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install, "README.md"), []byte("Automated security review for changes."), 0600); err != nil {
		t.Fatal(err)
	}
	installed := map[string]any{"plugins": map[string]any{"security-guidance@official": []any{map[string]any{"installPath": install}}}}
	data, err := json.Marshal(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(home, ".claude", "plugins", "installed_plugins.json")), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"enabledPlugins":{"security-guidance@official":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	writeOptimizeSession(t, projects, "hidden", []any{optimizeUser("Run security-review for sdk-py", "sdk-py", map[string]any{"promptSource": "sdk"}), optimizeAssistant("a", "sdk-py", 100, "ok", nil)}, 5)
	report := optimizeReport(t, projects, home, "", 30)
	row := optimizeHidden(report)[0]
	output := render(report)
	if row.source != "security-guidance@official" || row.status != "stopped" || row.lastSeen == "" || !strings.Contains(output, "security guidance: 1 hidden sessions") || !strings.Contains(output, "stopped: plugin disabled") || strings.Contains(output, "Review launcher") {
		t.Fatalf("hidden plugin report = %#v\n%s", row, output)
	}
}

func TestOptimizeEnabledPluginKeepsItsAction(t *testing.T) {
	root := t.TempDir()
	home, projects := filepath.Join(root, "home"), filepath.Join(root, "projects")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"enabledPlugins":{"design@official":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	writeOptimizeSession(t, projects, "s", []any{optimizeSkillListing([2]string{"design:unused", strings.Repeat("x", 2400)}), optimizeUser("hello", "cli", nil), optimizeAssistant("a", "cli", 100, "ok", nil)}, 0)
	output := render(optimizeReport(t, projects, home, "", 30))
	if !strings.Contains(output, "enabled but not observed") || !strings.Contains(output, "Disable plugin") {
		t.Fatalf("enabled plugin render = %s", output)
	}
}

func TestOptimizeCurrentPluginPresenceDoesNotOverrideDisabledSetting(t *testing.T) {
	root := t.TempDir()
	home, projects, project := filepath.Join(root, "home"), filepath.Join(root, "projects"), filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"enabledPlugins":{"design@official":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	assistant := optimizeAssistant("a", "cli", 100, "ok", []map[string]any{optimizeTool("mcp__plugin:design:server__query", nil, "mcp"), optimizeTool("plugin:design:other", nil, "plugin")})
	assistant["cwd"] = project
	writeOptimizeSession(t, projects, "s", []any{optimizeSkillListing([2]string{"design:unused", strings.Repeat("x", 2400)}), optimizeUser("hello", "cli", map[string]any{"cwd": project}), assistant}, 0)
	report := optimizeReport(t, projects, home, "", 30)
	row := optimizeRow(optimizeStartup(report), "plugin:design")
	if row.status != "stopped" || row.observedLoaded || row.lastSeen != "" || strings.Contains(render(report), "disabled in the current configuration but was loaded") {
		t.Fatalf("current plugin report = %#v\n%s", row, render(report))
	}
}

func TestOptimizeStoppedHiddenSourcesAreSummarizedAfterTable(t *testing.T) {
	output := optimizeRenderReport(nil, []hiddenRow{{entrypoint: "sdk-py", sessions: 174, weightedTokens: 21_200_000, status: "stopped", source: "security-guidance@official", lastSeen: "2026-09-25"}}, nil, 30)
	tableEnd := strings.Index(output, "| 1 |")
	summary := "> *Already stopped: security guidance hidden reviews (~21.2M tokens, last seen 2026-09-25).*"
	if tableEnd < 0 || strings.Index(output, summary) <= tableEnd || strings.Count(output, "Already stopped:") != 1 {
		t.Fatalf("stopped hidden render = %s", output)
	}
}

func TestOptimizeUnknownStatusQuoteTableAndActiveRanking(t *testing.T) {
	output := optimizeRenderReport([]startupItem{
		{name: "plugin:stopped", kind: "plugin", tokensPerRequest: 100, requestsLoaded: 10, flagged: true, status: "stopped", lastSeen: "2026-09-25"},
		{name: "plugin:active", kind: "plugin", tokensPerRequest: 100, requestsLoaded: 10, flagged: true, status: "active", lastSeen: "2026-09-30"},
		{name: "plugin:mystery", kind: "plugin", tokensPerRequest: 100, requestsLoaded: 10, flagged: true, status: "unknown"},
	}, nil, nil, 30)
	lines := strings.Split(output, "\n")
	if lines[0] != "> ⚓ **Purser optimize**" || !containsLine(lines, "> | # | Finding | Cost | Status | Action |") {
		t.Fatalf("table heading = %s", output)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "> ") {
			t.Fatalf("unquoted output line = %q", line)
		}
	}
	if !(strings.Index(output, "active plugin") < strings.Index(output, "mystery plugin") && strings.Index(output, "mystery plugin") < strings.Index(output, "stopped plugin")) || !strings.Contains(output, "status unknown") || !strings.Contains(output, "stopped: plugin disabled · last seen 2026-09-25") {
		t.Fatalf("status ranking render = %s", output)
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestOptimizeEmptyStateLineAndLearningFooter(t *testing.T) {
	output := optimizeRenderReport(nil, nil, nil, 30)
	if !strings.Contains(output, "*No clear token waste found in your recent sessions.*") || strings.Contains(output, "1.") || !strings.HasSuffix(output, "\n> \n> *🧠 Purser learns as it works in this project: it measures how long its work really takes, sharpens its estimates, and points out where tokens and time can be saved.*") {
		t.Fatalf("empty render = %s", output)
	}
}
