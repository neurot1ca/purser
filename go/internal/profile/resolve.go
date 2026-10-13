package profile

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

var engineProtected = []string{
	".orchestrator/**", "AGENTS.md", "CLAUDE.md", ".claude/settings.json",
	".claude/settings.local.json", ".git", ".git/**", ".env", ".env.*",
	"*.pem", "*.key", "credentials.*", "secrets.*",
}

// EngineProtectedPaths returns the engine's default protected path patterns.
func EngineProtectedPaths() []string {
	return append([]string(nil), engineProtected...)
}

type Roles struct {
	Orchestrator string `json:"orchestrator"`
	Coder        string `json:"coder"`
	Reviewer     string `json:"reviewer"`
}

type ReviewerCandidate struct {
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

type WorktreeSetup struct {
	Copy                []string `json:"copy"`
	Setup               *string  `json:"setup"`
	SetupTimeoutSeconds int      `json:"setup_timeout_seconds"`
}

// ResolvedProfile contains the fields returned by project_profile._resolve_profile.
type ResolvedProfile struct {
	Configured bool    `json:"configured"`
	ProjectID  *string `json:"project_id"`
	// MeasurementLedger is configured as [project].measurement_ledger, optionally with #Heading.
	MeasurementLedger  *string             `json:"measurement_ledger,omitempty"`
	ValidationCommand  *string             `json:"validation_command,omitempty"`
	ProtectedPaths     []string            `json:"protected_paths"`
	ForceFullReview    bool                `json:"force_full_review"`
	FullSuiteRequired  bool                `json:"full_suite_required"`
	CoverageTargetPct  float64             `json:"-"`
	SkipMaxLines       int                 `json:"skip_max_lines"`
	SkipMaxFiles       int                 `json:"skip_max_files"`
	Trusted            bool                `json:"trusted"`
	ReviewerExecutable *string             `json:"reviewer_executable"`
	ReviewerCandidates []ReviewerCandidate `json:"reviewer_candidates"`
	Roles              Roles               `json:"roles"`
	AuthoritySources   []AuthoritySource   `json:"authority_sources"`
	Checks             []Check             `json:"checks"`
	Worktree           WorktreeSetup       `json:"worktree"`
	VaultRoot          *string             `json:"vault_root"`
	LooseningsIgnored  int                 `json:"loosenings_ignored"`
	UserStatus         string              `json:"user_status"`
	ProjectStatus      string              `json:"project_status"`
	Warnings           []string            `json:"warnings"`
}

// ResolveProfile mirrors project_profile._resolve_profile for decoded tiers.
func ResolveProfile(projectRoot string, user, project Tier) ResolvedProfile {
	userValues, projectValues := map[string]any{}, map[string]any{}
	if user.Status == "ok" {
		userValues = user.Values
	}
	if project.Status == "ok" {
		projectValues = project.Values
	}
	forceFull, lines, files := false, 50, 2
	if value, ok := userValues["review.force_full"].(bool); ok {
		forceFull = value
	}
	if value, ok := intProfile(userValues["review.skip_max_lines"]); ok {
		lines = value
	}
	if value, ok := intProfile(userValues["review.skip_max_files"]); ok {
		files = value
	}
	ignored := 0
	if value, ok := intProfile(projectValues["review.skip_max_lines"]); ok {
		if value <= lines {
			lines = value
		} else {
			ignored++
		}
	}
	if value, ok := intProfile(projectValues["review.skip_max_files"]); ok {
		if value <= files {
			files = value
		} else {
			ignored++
		}
	}
	protectedSet := map[string]bool{}
	for _, value := range engineProtected {
		protectedSet[value] = true
	}
	for _, values := range []map[string]any{userValues, projectValues} {
		for _, path := range profileStrings(values["paths.protected"]) {
			protectedSet[path] = true
		}
	}
	protected := make([]string, 0, len(protectedSet))
	for path := range protectedSet {
		protected = append(protected, path)
	}
	sort.Strings(protected)
	warnings := append(append([]string{}, user.Warnings...), project.Warnings...)
	if user.Status == "rejected" || project.Status == "rejected" {
		forceFull = true
	} else if value, ok := projectValues["review.force_full"].(bool); ok {
		forceFull = forceFull || value
	}
	trusted := IsTrusted(projectRoot, user)
	if _, ok := projectValues["checks"]; ok && !trusted {
		warnings = append(warnings, "project: [[checks]] ignored because the project is not trusted")
	}
	if hasPrefixKey(projectValues, "worktree.") && !trusted {
		warnings = append(warnings, "project: [worktree] ignored because the project is not trusted")
	}
	sources, ok := projectValues["authority.sources"].([]AuthoritySource)
	if !ok || len(sources) == 0 {
		sources = DefaultAuthoritySources()
	}
	reviewer := "agy"
	if value, ok := userValues["roles.reviewer"].(string); ok {
		reviewer = value
	}
	if reviewer == "none" {
	} else if selected, ok := userValues["reviewer.selected"].(map[string]any); ok {
		if agent, valid := selected["agent"].(string); valid {
			reviewer = agent
		}
	}
	orchestrator := profileString(userValues, "roles.orchestrator", "claude-code")
	coder := profileString(userValues, "roles.coder", "codex")
	roles := Roles{Orchestrator: orchestrator, Coder: coder, Reviewer: reviewer}
	selected, selectedOK := userValues["reviewer.selected"].(map[string]any)
	alternatives, alternativesOK := userValues["reviewer.alternatives"].([]any)
	candidates := []ReviewerCandidate{}
	if selectedOK {
		candidates = append(candidates, ReviewerCandidate{Agent: selected["agent"].(string), Model: selected["model"].(string), Effort: profileString(selected, "effort", "")})
	} else {
		candidates = append(candidates, ReviewerCandidate{Agent: reviewer, Model: "gemini-3.8-flash-high"})
		if reviewer == "none" {
			candidates[0].Agent = "agy"
		}
	}
	if alternativesOK {
		for _, item := range alternatives {
			candidate := item.(map[string]any)
			candidates = append(candidates, ReviewerCandidate{Agent: candidate["agent"].(string), Model: candidate["model"].(string), Effort: profileString(candidate, "effort", "")})
		}
	}
	if !selectedOK && !alternativesOK {
		candidates = append(candidates,
			ReviewerCandidate{Agent: "claude", Model: "claude-haiku-5-5"},
			ReviewerCandidate{Agent: "codex", Model: "gpt-6-luna", Effort: "low"},
		)
	}
	worktree := WorktreeSetup{Copy: []string{}, SetupTimeoutSeconds: 600}
	if trusted {
		worktree.Copy = profileStrings(projectValues["worktree.copy"])
		worktree.Setup = profileOptionalString(projectValues["worktree.setup"])
		if value, ok := intProfile(projectValues["worktree.setup_timeout_seconds"]); ok {
			worktree.SetupTimeoutSeconds = value
		}
	}
	checks := []Check{}
	if trusted {
		checks = profileChecks(projectValues["checks"])
	}
	coverageTarget := 90.0
	if value, ok := projectValues["maintenance.coverage_target_pct"].(float64); ok {
		coverageTarget = value
	}
	return ResolvedProfile{Configured: project.Status != "missing", ProjectID: profileOptionalString(projectValues["project.id"]), MeasurementLedger: profileOptionalString(projectValues["project.measurement_ledger"]), ValidationCommand: profileOptionalString(projectValues["validation.command"]), ProtectedPaths: protected, ForceFullReview: forceFull, FullSuiteRequired: profileBool(projectValues, "review.full_suite_required"), CoverageTargetPct: coverageTarget, SkipMaxLines: lines, SkipMaxFiles: files, Trusted: trusted, ReviewerExecutable: profileOptionalString(userValues["reviewer.executable"]), ReviewerCandidates: candidates, Roles: roles, AuthoritySources: sources, Checks: checks, Worktree: worktree, VaultRoot: profileOptionalString(userValues["telemetry.vault_root"]), LooseningsIgnored: ignored, UserStatus: user.Status, ProjectStatus: project.Status, Warnings: warnings}
}

// IsTrusted requires a canonical path equality, never a parent/child match.
func IsTrusted(projectRoot string, user Tier) bool {
	if user.Status != "ok" {
		return false
	}
	root, err := canonicalPath(projectRoot)
	if err != nil {
		return false
	}
	for _, path := range profileStrings(user.Values["trust.paths"]) {
		candidate, err := canonicalPath(path)
		if err == nil && pathKey(root) == pathKey(candidate) {
			return true
		}
	}
	return false
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}
func pathKey(path string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}

// IsProtected mirrors project_profile.is_protected, including Python fnmatch's
// separator-agnostic wildcards and the special trailing /** prefix rule.
func IsProtected(path string, patterns []string) bool {
	normalized := strings.ReplaceAll(path, `\`, "/")
	for strings.HasPrefix(normalized, "./") {
		normalized = strings.TrimPrefix(normalized, "./")
	}
	components := strings.FieldsFunc(normalized, func(r rune) bool { return r == '/' })
	for _, raw := range patterns {
		pattern := strings.ReplaceAll(raw, `\`, "/")
		matchPath, matchPattern := normalized, pattern
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			matchPath, matchPattern = strings.ToLower(matchPath), strings.ToLower(matchPattern)
		}
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimRight(matchPattern[:len(matchPattern)-3], "/")
			if matchPath == prefix || strings.HasPrefix(matchPath, prefix+"/") {
				return true
			}
			continue
		}
		if strings.Contains(pattern, "/") {
			if fnmatch(matchPath, matchPattern) {
				return true
			}
			continue
		}
		for _, component := range components {
			if fnmatch(strings.ToLower(component), strings.ToLower(pattern)) {
				return true
			}
		}
	}
	return false
}

func fnmatch(name, pattern string) bool {
	n, p := []rune(name), []rune(pattern)
	memo := map[[2]int]bool{}
	seen := map[[2]int]bool{}
	var match func(int, int) bool
	match = func(i, j int) bool {
		key := [2]int{i, j}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		result := false
		if j == len(p) {
			result = i == len(n)
		} else if p[j] == '*' {
			result = match(i, j+1) || i < len(n) && match(i+1, j)
		} else if i < len(n) && p[j] == '?' {
			result = match(i+1, j+1)
		} else if i < len(n) && p[j] == '[' {
			end, negate, members, valid := charClass(p, j)
			if valid {
				inside := runeInClass(n[i], members)
				if negate {
					inside = !inside
				}
				result = inside && match(i+1, end+1)
			} else {
				result = n[i] == '[' && match(i+1, j+1)
			}
		} else if i < len(n) && p[j] == n[i] {
			result = match(i+1, j+1)
		}
		memo[key] = result
		return result
	}
	return match(0, 0)
}
func charClass(pattern []rune, start int) (int, bool, []rune, bool) {
	end := start + 1
	if end < len(pattern) && (pattern[end] == '!' || pattern[end] == '^') {
		end++
	}
	if end < len(pattern) && pattern[end] == ']' {
		end++
	}
	for end < len(pattern) && pattern[end] != ']' {
		end++
	}
	if end == len(pattern) {
		return 0, false, nil, false
	}
	begin := start + 1
	negate := pattern[begin] == '!' || pattern[begin] == '^'
	if negate {
		begin++
	}
	return end, negate, pattern[begin:end], true
}
func runeInClass(value rune, members []rune) bool {
	for i := 0; i < len(members); i++ {
		if i+2 < len(members) && members[i+1] == '-' {
			if members[i] <= value && value <= members[i+2] {
				return true
			}
			i += 2
			continue
		}
		if members[i] == value {
			return true
		}
	}
	return false
}

func intProfile(value any) (int, bool) {
	return integerOnly(value)
}
func profileStrings(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string{}, values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, item := range values {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	}
	return []string{}
}

func profileChecks(value any) []Check {
	if checks, ok := value.([]Check); ok {
		return checks
	}
	items, ok := value.([]any)
	if !ok {
		return []Check{}
	}
	checks := make([]Check, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		argv := profileStrings(entry["argv"])
		var when []string
		if value, exists := entry["when"]; exists && value != nil {
			when = profileStrings(value)
		}
		timeout := 60
		if value, valid := intProfile(entry["timeout_seconds"]); valid {
			timeout = value
		}
		checks = append(checks, Check{Name: name, Argv: argv, When: when, TimeoutSeconds: timeout})
	}
	return checks
}
func profileOptionalString(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}
func profileString(values map[string]any, key, fallback string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return fallback
}
func profileBool(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}
func hasPrefixKey(values map[string]any, prefix string) bool {
	for key := range values {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
