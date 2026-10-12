package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"purser-hook/internal/proc"
)

var authorityHeadings = []string{"Authority", "Working scope", "Proportional validation", "Test and comment quality"}
var markdownHeading = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
var markdownHeadingStart = regexp.MustCompile(`^(#{1,6})\s+`)
var gitExecutable = "git"

func init() {
	if resolved, err := exec.LookPath("git"); err == nil {
		gitExecutable = resolved
	}
}

// ValidateConfig applies the Reviewer-1 config checks performed by reviewer.py.
func ValidateConfig(config map[string]any) error {
	model, ok := config["reviewer1_model_requested"].(string)
	if !ok || model == "" {
		return fmt.Errorf("reviewer1_model_requested must be a non-empty string")
	}
	agent, hasAgent := config["reviewer1_agent"]
	if !hasAgent || agent == "agy" {
		if err := (reviewerAdapter{"agy"}).ValidateModel(model); err != nil {
			return err
		}
	}
	timeout, ok := config["reviewer1_timeout_seconds"].(float64)
	if !ok {
		if integer, valid := config["reviewer1_timeout_seconds"].(int); valid {
			timeout, ok = float64(integer), true
		}
		if number, valid := config["reviewer1_timeout_seconds"].(json.Number); valid {
			timeout, _ = number.Float64()
			ok = true
		}
	}
	if !ok || math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
		return fmt.Errorf("reviewer1_timeout_seconds must be positive")
	}
	budget, ok := config["reviewer1_packet_max_bytes"].(int)
	if !ok {
		if number, valid := config["reviewer1_packet_max_bytes"].(json.Number); valid {
			if parsed, err := number.Int64(); err == nil && parsed > 0 && parsed <= int64(^uint(0)>>1) {
				budget, ok = int(parsed), true
			}
		}
	}
	if !ok || budget <= 0 {
		return fmt.Errorf("reviewer1_packet_max_bytes must be a positive integer")
	}
	return nil
}

// BuildMaterials recreates the Python reviewer seam's material mapping.
func BuildMaterials(taskID, repoRoot string, delta, classification map[string]any, validationSummary, taskObjective, acceptanceCriteria *string, authoritySources []any) (map[string]any, error) {
	paths, diff, err := delegatedDiff(repoRoot, delta, classification)
	if err != nil {
		return nil, err
	}
	changed, coverage := reviewCoverage(delta, paths)
	if coverage.unknown {
		// Keep the material packet usable for the attributable diff, but make the
		// loss of a trustworthy coverage comparison explicit to the next reviewer.
		changed = nil
	}
	if len(changed) > 0 {
		outsideDiff, outsideErr := outOfScopeDiff(repoRoot, delta, changed, paths)
		if outsideErr != nil {
			return nil, outsideErr
		}
		diff += outsideDiff
	}
	var decision any = "UNKNOWN"
	if classification != nil {
		if value, ok := classification["decision"]; ok {
			decision = value
		}
	}
	validationEvidence := ""
	if validationSummary == nil {
		validationEvidence = "No orchestrator validation evidence was supplied to this review. This is the normal review-seam default and is not, by itself, grounds for UNCERTAIN."
	} else if strings.TrimSpace(*validationSummary) == "" {
		return nil, fmt.Errorf("validation evidence must be non-empty text when supplied")
	} else {
		value := strings.TrimSpace(*validationSummary)
		if strings.HasPrefix(value, "Worker-reported") || strings.HasPrefix(value, "Orchestrator-") {
			validationEvidence = value
		} else {
			validationEvidence = "Orchestrator-supplied validation evidence: " + value
		}
	}
	objective := fmt.Sprintf("Review the complete delegated change for task %s, bounded to these actual changed paths: %s.", pythonStringRepr(taskID), strings.Join(paths, ", "))
	if taskObjective != nil && strings.TrimSpace(*taskObjective) != "" {
		objective += " Task Objective: " + strings.TrimSpace(*taskObjective)
	}
	criteria := "Accept only if the complete delegated diff is correct within the named bounded task, preserves current contracts, and introduces no material regression or scope expansion. Consider supplied validation evidence, but do not treat its normal absence as evidence that the diff is incomplete."
	if acceptanceCriteria != nil && strings.TrimSpace(*acceptanceCriteria) != "" {
		criteria += " Specific Task Criteria: " + strings.TrimSpace(*acceptanceCriteria)
	}
	if coverage.unknown {
		validationEvidence += " REVIEW COVERAGE UNKNOWN: changed paths could not be safely compared with the reviewed paths. FULL Claude review is required."
	} else if len(coverage.outside) > 0 {
		validationEvidence += " PARTIAL REVIEW COVERAGE: Reviewer-1 receives in-scope paths and the out-of-scope diff marked [[OUT OF SCOPE]] for: " + strings.Join(coverage.outside, ", ") + ". FULL Claude review is required."
	}
	weakening := TestWeakeningCandidates(diff)
	if len(weakening) > 0 {
		validationEvidence += " Test-change candidates (unconfirmed; inspect the cited diff hunk): " + strings.Join(weakening, "; ")
	}
	authority, err := authoritySlice(repoRoot, paths, authoritySources)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"objective":           objective,
		"acceptance_criteria": criteria,
		"authority_contract":  authority,
		"delegated_diff":      diff,
		"validation_summary":  fmt.Sprintf("%s Ledger delta: %s files, %s changed lines; concurrent-writer decision: %s. Raw agent output and full success logs were not forwarded.", validationEvidence, pythonValueString(delta["changed_file_count"]), pythonValueString(delta["changed_line_count"]), pythonValueString(decision)),
	}, nil
}

type reviewCoverageResult struct {
	outside []string
	unknown bool
}

func reviewCoverage(delta map[string]any, reviewed []string) ([]string, reviewCoverageResult) {
	changed, err := safeDeltaPaths(delta)
	if err != nil {
		return nil, reviewCoverageResult{unknown: true}
	}
	changedSet := map[string]bool{}
	for _, path := range changed {
		changedSet[path] = true
	}
	reviewedSet := map[string]bool{}
	for _, path := range reviewed {
		if !changedSet[path] {
			return nil, reviewCoverageResult{unknown: true}
		}
		reviewedSet[path] = true
	}
	var outside []string
	for _, path := range changed {
		if !reviewedSet[path] {
			outside = append(outside, path)
		}
	}
	return changed, reviewCoverageResult{outside: outside}
}

func outOfScopeDiff(repo string, delta map[string]any, changed, reviewed []string) (string, error) {
	_, coverage := reviewCoverage(map[string]any{"changed_paths": changed}, reviewed)
	if coverage.unknown || len(coverage.outside) == 0 {
		return "", nil
	}
	baseline, ok := delta["baseline_head"].(string)
	if !ok || baseline == "" {
		return "", fmt.Errorf("out-of-scope changed files have no baseline head")
	}
	diff, err := completePathDiff(repo, baseline, coverage.outside)
	if err != nil {
		return "", err
	}
	return markOutOfScopeDiff(diff), nil
}

func completePathDiff(repo, baseline string, paths []string) (string, error) {
	pathspecs := make([]string, len(paths))
	for i, path := range paths {
		pathspecs[i] = ":(literal)" + path
	}
	args := append([]string{"diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", baseline, "--"}, pathspecs...)
	stdout, err := runGit(repo, args...)
	if err != nil {
		return "", err
	}
	names := linesSet(stdout)
	args = append([]string{"diff", "--binary", "--no-renames", "--no-ext-diff", "--no-textconv", baseline, "--"}, pathspecs...)
	tracked, err := runGit(repo, args...)
	if err != nil {
		return "", err
	}
	var additions strings.Builder
	for _, path := range paths {
		if !names[path] {
			addition, err := newFileDiff(repo, path)
			if err != nil {
				return "", err
			}
			additions.WriteString(addition)
		}
	}
	diff := tracked + additions.String()
	if diff == "" {
		return "", fmt.Errorf("the out-of-scope changed files produced no reviewable diff")
	}
	return diff, nil
}

func markOutOfScopeDiff(diff string) string {
	var marked strings.Builder
	for _, line := range strings.SplitAfter(diff, "\n") {
		marked.WriteString(line)
		if strings.HasPrefix(line, "diff --git ") {
			marked.WriteString("[[OUT OF SCOPE]]\n")
		}
	}
	return marked.String()
}

// TestWeakeningCandidates reports narrow textual candidates in test-file diffs.
// It deliberately does not infer weakening when an assertion is replaced or relocated.
func TestWeakeningCandidates(diff string) []string {
	type change struct{ removed, added, hunks []string }
	files := map[string]*change{}
	path, hunk := "", ""
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git a/") {
			parts := strings.SplitN(strings.TrimPrefix(line, "diff --git a/"), " b/", 2)
			path, hunk = "", ""
			if len(parts) == 2 {
				path = parts[1]
			}
			if isTestDiffPath(path) {
				files[path] = &change{}
			}
			continue
		}
		if strings.HasPrefix(line, "@@") {
			hunk = strings.Fields(line)[0]
			continue
		}
		current := files[path]
		if current == nil || len(line) == 0 {
			continue
		}
		if line[0] == '-' && !strings.HasPrefix(line, "--- ") {
			current.removed = append(current.removed, line[1:])
			current.hunks = append(current.hunks, hunk)
		} else if line[0] == '+' && !strings.HasPrefix(line, "+++ ") {
			current.added = append(current.added, line[1:])
		}
	}
	var findings []string
	for name, current := range files {
		for i, removed := range current.removed {
			if assertionLine(removed) && !containsAssertion(current.added) && !containsExactLine(current.added, removed) {
				findings = append(findings, "TEST_ASSERTION_REMOVED candidate at "+name+" "+current.hunks[i])
				break
			}
		}
		for _, added := range current.added {
			lower := strings.ToLower(added)
			if strings.Contains(lower, "skip(") || strings.Contains(lower, ".skip") || strings.Contains(lower, "xfail") || strings.Contains(lower, "expectedfailure") {
				findings = append(findings, "TEST_SKIP_ADDED candidate at "+name)
				break
			}
		}
	}
	sort.Strings(findings)
	return findings
}

func isTestDiffPath(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "test_") || strings.Contains(name, ".test.") || strings.Contains(name, ".spec.")
}

func assertionLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "assert") || strings.Contains(lower, "assert.") || strings.Contains(lower, "t.error") || strings.Contains(lower, "t.fatal") || strings.Contains(lower, "require.")
}

func containsAssertion(lines []string) bool {
	for _, line := range lines {
		if assertionLine(line) {
			return true
		}
	}
	return false
}

func containsExactLine(lines []string, target string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == strings.TrimSpace(target) {
			return true
		}
	}
	return false
}

func authoritySlice(repoRoot string, changed []string, sources []any) (string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if sources == nil {
		sources = []any{map[string]any{"path": "AGENTS.md", "headings": authorityHeadings}}
	}
	var slices []string
	for _, raw := range sources {
		path, headings, whenChanged, valid := readAuthoritySource(raw)
		if !valid || unsafeAuthorityPath(path) {
			continue
		}
		normalized := strings.ReplaceAll(path, `\`, "/")
		for strings.HasPrefix(normalized, "./") {
			normalized = strings.TrimPrefix(normalized, "./")
		}
		if normalized == "" || strings.HasPrefix(normalized, "/") {
			continue
		}
		if whenChanged != nil {
			match := false
			prefix := strings.ReplaceAll(*whenChanged, `\`, "/")
			for _, p := range changed {
				if strings.HasPrefix(strings.ReplaceAll(p, `\`, "/"), prefix) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		candidate := filepath.Join(root, filepath.FromSlash(normalized))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if !withinPath(resolved, root) {
			continue
		}
		text := markdownSections(resolved, headings)
		if text != "" {
			slices = append(slices, text)
		}
	}
	if len(slices) == 0 {
		return "No readable repository authority slice was available; treat authority as UNKNOWN.", nil
	}
	return strings.Join(slices, "\n\n"), nil
}

func readAuthoritySource(raw any) (string, []string, *string, bool) {
	var path any
	var headingsRaw any
	var when any
	switch source := raw.(type) {
	case map[string]any:
		path, headingsRaw, when = source["path"], source["headings"], source["when_changed"]
	default:
		return "", nil, nil, false
	}
	name, ok := path.(string)
	if !ok {
		return "", nil, nil, false
	}
	items, ok := headingsRaw.([]any)
	if !ok {
		if strings, yes := headingsRaw.([]string); yes {
			result := append([]string(nil), strings...)
			items = nil
			for _, v := range result {
				items = append(items, v)
			}
		} else {
			return "", nil, nil, false
		}
	}
	var headings []string
	if items != nil {
		for _, item := range items {
			heading, ok := item.(string)
			if !ok || heading == "" {
				return "", nil, nil, false
			}
			headings = append(headings, heading)
		}
	} else if values, ok := headingsRaw.([]string); ok {
		headings = values
	}
	var changed *string
	if when != nil {
		value, ok := when.(string)
		if !ok {
			return "", nil, nil, false
		}
		changed = &value
	}
	return name, headings, changed, true
}

func unsafeAuthorityPath(path string) bool {
	normalized := strings.ReplaceAll(path, `\`, "/")
	if filepath.IsAbs(path) || filepath.IsAbs(normalized) || strings.HasPrefix(normalized, "/") {
		return true
	}
	if len(normalized) >= 2 && ((normalized[0] >= 'A' && normalized[0] <= 'Z') || (normalized[0] >= 'a' && normalized[0] <= 'z')) && normalized[1] == ':' {
		return true
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func markdownSections(path string, headings []string) string {
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return ""
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	wanted := map[string]bool{}
	for _, heading := range headings {
		wanted[caseFold(heading)] = true
	}
	var selected []string
	for i := 0; i < len(lines); {
		match := markdownHeading.FindStringSubmatch(lines[i])
		if match == nil || !wanted[caseFold(strings.TrimSpace(match[2]))] {
			i++
			continue
		}
		level := len(match[1])
		selected = append(selected, lines[i])
		i++
		for i < len(lines) {
			next := markdownHeadingStart.FindStringSubmatch(lines[i])
			if next != nil && len(next[1]) <= level {
				break
			}
			selected = append(selected, lines[i])
			i++
		}
	}
	return strings.TrimSpace(strings.Join(selected, "\n"))
}

func caseFold(s string) string {
	return strings.Map(func(r rune) rune { return unicode.ToLower(r) }, s)
}

func pythonStringRepr(value string) string {
	quote := byte('\'')
	if strings.Contains(value, "'") && !strings.Contains(value, `"`) {
		quote = '"'
	}
	var result strings.Builder
	result.WriteByte(quote)
	for _, r := range value {
		switch r {
		case '\\':
			result.WriteString(`\\`)
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		case '\t':
			result.WriteString(`\t`)
		default:
			if byte(r) == quote {
				result.WriteByte('\\')
			}
			result.WriteRune(r)
		}
	}
	result.WriteByte(quote)
	return result.String()
}

func pythonValueString(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return v
	case json.Number:
		return string(v)
	case float64:
		formatted := strconv.FormatFloat(v, 'g', -1, 64)
		if !strings.ContainsAny(formatted, ".eE") {
			formatted += ".0"
		}
		return formatted
	case float32:
		return pythonValueString(float64(v))
	default:
		return fmt.Sprint(value)
	}
}

func safeDeltaPaths(delta map[string]any) ([]string, error) {
	raw, ok := delta["changed_paths"].([]any)
	if !ok {
		if values, yes := delta["changed_paths"].([]string); yes {
			raw = make([]any, len(values))
			for i, v := range values {
				raw[i] = v
			}
			ok = true
		}
	}
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("delta has no changed paths")
	}
	set := map[string]bool{}
	for _, value := range raw {
		path, ok := value.(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("delta contains an invalid path")
		}
		normalized := strings.ReplaceAll(path, `\`, "/")
		if filepath.IsAbs(normalized) || strings.HasPrefix(normalized, "/") {
			return nil, fmt.Errorf("delta contains an unsafe path")
		}
		for _, part := range strings.Split(normalized, "/") {
			if part == ".." {
				return nil, fmt.Errorf("delta contains an unsafe path")
			}
		}
		if normalized == "." || normalized == "" {
			return nil, fmt.Errorf("delta contains an unsafe path")
		}
		set[normalized] = true
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func runGit(repo string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitExecutable, args...)
	cmd.Dir = repo
	proc.ConfigureNoWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("git could not reconstruct the delegated diff")
	}
	if err != nil || !utf8.Valid(output) || !utf8.Valid(stderr.Bytes()) {
		return "", fmt.Errorf("git could not reconstruct the delegated diff")
	}
	return string(output), nil
}

func newFileDiff(repo, path string) (string, error) {
	current := filepath.Join(repo, filepath.FromSlash(path))
	resolved, err := filepath.EvalSymlinks(current)
	root, rootErr := filepath.EvalSymlinks(repo)
	if err != nil || rootErr != nil || !withinPath(resolved, root) {
		return "", fmt.Errorf("the complete new-file content is unavailable")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("the complete new-file content is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitExecutable, "diff", "--binary", "--no-index", "--no-renames", "--no-ext-diff", "--no-textconv", "--", os.DevNull, path)
	cmd.Dir = repo
	proc.ConfigureNoWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			code = -1
		}
	}
	if ctx.Err() != nil || (code != 0 && code != 1) || !utf8.Valid(output) || !utf8.Valid(stderr.Bytes()) {
		return "", fmt.Errorf("git could not reconstruct the complete new-file addition")
	}
	if len(output) > 0 {
		return string(output), nil
	}
	if code == 0 && info.Size() == 0 {
		return fmt.Sprintf("NEW FILE %q: complete current content is empty (0 bytes).\n", path), nil
	}
	return "", fmt.Errorf("git produced no complete new-file addition")
}

func sourcePaths(delta map[string]any, source string) (map[string]bool, error) {
	sources, _ := delta["sources"].(map[string]any)
	entry, _ := sources[source].(map[string]any)
	raw := entry["paths"]
	paths, ok := raw.([]any)
	if !ok {
		if values, yes := raw.([]string); yes {
			paths = make([]any, len(values))
			for i, v := range values {
				paths[i] = v
			}
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("delta source %q has invalid paths", source)
	}
	result := map[string]bool{}
	for _, value := range paths {
		path, ok := value.(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("delta source %q has invalid paths", source)
		}
		result[strings.ReplaceAll(path, `\`, "/")] = true
	}
	return result, nil
}

func reviewPaths(delta, classification map[string]any) ([]string, error) {
	changed, changedErr := safeDeltaPaths(delta)
	if classification == nil {
		return changed, changedErr
	}
	raw, ok := classification["in_scope_paths"].([]any)
	if !ok {
		if values, yes := classification["in_scope_paths"].([]string); yes {
			raw = make([]any, len(values))
			for i, v := range values {
				raw[i] = v
			}
			ok = true
		}
	}
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("concurrent-writer classification has no delegated paths")
	}
	selected, err := safeDeltaPaths(map[string]any{"changed_paths": raw})
	if err != nil {
		return nil, err
	}
	if changedErr != nil {
		return selected, nil
	}
	allowed := map[string]bool{}
	for _, p := range changed {
		allowed[p] = true
	}
	for _, p := range selected {
		if !allowed[p] {
			return nil, fmt.Errorf("concurrent-writer classification escaped the known delta")
		}
	}
	return selected, nil
}

func delegatedDiff(repo string, delta, classification map[string]any) ([]string, string, error) {
	paths, err := reviewPaths(delta, classification)
	if err != nil {
		return nil, "", err
	}
	baseline, ok := delta["baseline_head"].(string)
	if !ok || baseline == "" {
		return nil, "", fmt.Errorf("delta has no baseline head")
	}
	ambiguous, ok := delta["ambiguous_paths"].([]any)
	if !ok {
		if values, yes := delta["ambiguous_paths"].([]string); yes {
			ambiguous = make([]any, len(values))
			for i, v := range values {
				ambiguous[i] = v
			}
			ok = true
		}
	}
	if !ok {
		return nil, "", fmt.Errorf("delta has invalid ambiguity evidence")
	}
	pathSet := map[string]bool{}
	for _, p := range paths {
		pathSet[p] = true
	}
	var overlap []string
	for _, value := range ambiguous {
		p, ok := value.(string)
		if !ok {
			return nil, "", fmt.Errorf("delta has invalid ambiguity evidence")
		}
		p = strings.ReplaceAll(p, `\`, "/")
		if pathSet[p] {
			overlap = append(overlap, p)
		}
	}
	sort.Strings(overlap)
	if len(overlap) > 0 {
		return nil, "", fmt.Errorf("the delegated diff cannot be isolated from pre-existing same-path changes: %s", strings.Join(overlap, ", "))
	}
	manifest, err := sourcePaths(delta, "scoped_manifest")
	if err != nil {
		return nil, "", err
	}
	manifestPaths := map[string]bool{}
	for _, p := range paths {
		if manifest[p] {
			manifestPaths[p] = true
		}
	}
	pathspecs := make([]string, len(paths))
	for i, p := range paths {
		pathspecs[i] = ":(literal)" + p
	}
	args := append([]string{"ls-tree", "-r", "--name-only", baseline, "--"}, pathspecs...)
	stdout, err := runGit(repo, args...)
	if err != nil {
		return nil, "", err
	}
	baselineNames := linesSet(stdout)
	var unresolved []string
	for p := range manifestPaths {
		if baselineNames[p] {
			unresolved = append(unresolved, p)
		}
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		return nil, "", fmt.Errorf("the accepted seam has hashes but no baseline contents for changed untracked or ignored paths: %s", strings.Join(unresolved, ", "))
	}
	newAtBaseline := map[string]bool{}
	for p := range manifestPaths {
		if !baselineNames[p] {
			newAtBaseline[p] = true
		}
	}
	args = append([]string{"diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", baseline, "--"}, pathspecs...)
	stdout, err = runGit(repo, args...)
	if err != nil {
		return nil, "", err
	}
	names := linesSet(stdout)
	omitted := map[string]bool{}
	for _, p := range paths {
		if !names[p] {
			omitted[p] = true
		}
	}
	var unavailable []string
	for p := range omitted {
		if !newAtBaseline[p] {
			unavailable = append(unavailable, p)
		}
	}
	sort.Strings(unavailable)
	var outside []string
	for p := range names {
		if !pathSet[p] {
			outside = append(outside, p)
		}
	}
	sort.Strings(outside)
	if len(unavailable) > 0 || len(outside) > 0 {
		values := unavailable
		if len(values) == 0 {
			values = outside
		}
		return nil, "", fmt.Errorf("the complete delegated diff is unavailable for: %s", strings.Join(values, ", "))
	}
	args = append([]string{"diff", "--binary", "--no-renames", "--no-ext-diff", "--no-textconv", baseline, "--"}, pathspecs...)
	tracked, err := runGit(repo, args...)
	if err != nil {
		return nil, "", err
	}
	omittedPaths := make([]string, 0, len(omitted))
	for p := range omitted {
		omittedPaths = append(omittedPaths, p)
	}
	sort.Strings(omittedPaths)
	var additions strings.Builder
	for _, p := range omittedPaths {
		addition, err := newFileDiff(repo, p)
		if err != nil {
			return nil, "", err
		}
		additions.WriteString(addition)
	}
	diff := tracked + additions.String()
	if diff == "" {
		return nil, "", fmt.Errorf("the computed non-empty delta produced no reviewable diff")
	}
	return paths, diff, nil
}

// DelegatedDiff returns only the complete attributed addition selected from a
// filtered job delta. Readiness consumes the same attribution boundary as review.
func DelegatedDiff(repo string, delta map[string]any) ([]string, string, error) {
	return delegatedDiff(repo, delta, nil)
}

func linesSet(text string) map[string]bool {
	result := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			result[strings.ReplaceAll(line, `\`, "/")] = true
		}
	}
	return result
}

func withinPath(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
