package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func loadProjectProfileTest(t *testing.T, project, user string) ResolvedProfile {
	t.Helper()
	root := t.TempDir()
	if project != "" {
		path := filepath.Join(root, ".orchestrator", "profile.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.ReplaceAll(project, "$ROOT", filepath.ToSlash(root))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	userPath := filepath.Join(root, "user.toml")
	if user != "" {
		if err := os.WriteFile(userPath, []byte(strings.ReplaceAll(user, "$ROOT", filepath.ToSlash(root))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return LoadProfile(root, userPath)
}

func TestProjectProfileValidChecksAndAbsentSection(t *testing.T) {
	plain := loadProjectProfileTest(t, "schema_version = 1\n", "")
	if plain.ProjectStatus != "ok" || len(plain.Checks) != 0 {
		t.Fatalf("absent checks: status=%q checks=%#v", plain.ProjectStatus, plain.Checks)
	}
	valid := loadProjectProfileTest(t, `schema_version = 1
[[checks]]
name = "lint"
argv = ["npm", "run", "lint"]
when = ["**/*.ts", "app/**"]
timeout_seconds = 30
[[checks]]
name = "test"
argv = ["python", "-V"]
`, `schema_version = 1
[trust]
paths = ["$ROOT"]
`)
	want := []Check{
		{Name: "lint", Argv: []string{"npm", "run", "lint"}, When: []string{"**/*.ts", "app/**"}, TimeoutSeconds: 30},
		{Name: "test", Argv: []string{"python", "-V"}, TimeoutSeconds: 60},
	}
	if valid.ProjectStatus != "ok" || !valid.Trusted || !reflect.DeepEqual(valid.Checks, want) {
		t.Fatalf("valid checks: status=%q trusted=%v checks=%#v, want %#v", valid.ProjectStatus, valid.Trusted, valid.Checks, want)
	}
	for _, warning := range valid.Warnings {
		if strings.Contains(warning, "unknown") {
			t.Fatalf("unexpected unknown warning: %q", warning)
		}
	}
}

func TestProjectProfileNonBooleanFullSuiteRequiredRejectsTier(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n[review]\nfull_suite_required = 1\n", "")
	if result.ProjectStatus != "rejected" || result.FullSuiteRequired || !result.ForceFullReview {
		t.Fatalf("non-boolean full_suite_required result: %+v", result)
	}
}

func TestProjectProfileWorktreeValuesAndDefaults(t *testing.T) {
	result := loadProjectProfileTest(t, `schema_version = 1
[worktree]
copy = [".env", "config/local.json"]
setup = "npm ci"
`, `schema_version = 1
[trust]
paths = ["$ROOT"]
`)
	want := WorktreeSetup{Copy: []string{".env", "config/local.json"}, Setup: stringPointer("npm ci"), SetupTimeoutSeconds: 600}
	if result.ProjectStatus != "ok" || !reflect.DeepEqual(result.Worktree, want) {
		t.Fatalf("worktree=%+v, want %+v (project status %q)", result.Worktree, want, result.ProjectStatus)
	}
}

func TestProjectProfileReviewerSelectedAgentAccepted(t *testing.T) {
	for _, agent := range []string{"opencode", "codex"} {
		t.Run(agent, func(t *testing.T) {
			result := loadProjectProfileTest(t, "schema_version = 1\n", fmt.Sprintf(`schema_version = 1
[roles]
reviewer = %q
[reviewer.selected]
agent = %q
model = "deepseek/deepseek-flash"
`, agent, agent))
			if result.UserStatus != "ok" || result.Roles.Reviewer != agent || len(result.ReviewerCandidates) == 0 || result.ReviewerCandidates[0].Agent != agent {
				t.Fatalf("selected reviewer %q: status=%q roles=%+v candidates=%+v", agent, result.UserStatus, result.Roles, result.ReviewerCandidates)
			}
		})
	}
}

func TestProjectProfileInvalidWorktreeFieldsWarnAndAreIgnored(t *testing.T) {
	result := loadProjectProfileTest(t, `schema_version = 1
[worktree]
copy = "../outside"
setup = 12
setup_timeout_seconds = 0
`, `schema_version = 1
[trust]
paths = ["$ROOT"]
`)
	if result.ProjectStatus != "ok" || !reflect.DeepEqual(result.Worktree, WorktreeSetup{Copy: []string{}, SetupTimeoutSeconds: 600}) {
		t.Fatalf("invalid worktree result: status=%q worktree=%+v", result.ProjectStatus, result.Worktree)
	}
	warnings := 0
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "[worktree]") {
			warnings++
		}
	}
	if warnings != 3 {
		t.Fatalf("got %d worktree warnings, want 3: %v", warnings, result.Warnings)
	}
}

func TestProjectProfileUntrustedChecksAreIgnored(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n[[checks]]\nname = \"lint\"\nargv = [\"python\", \"-V\"]\n", "")
	if result.ProjectStatus != "ok" || result.Trusted || len(result.Checks) != 0 {
		t.Fatalf("untrusted checks result: %+v", result)
	}
	if !containsString(result.Warnings, "project: [[checks]] ignored because the project is not trusted") {
		t.Fatalf("missing untrusted checks warning: %v", result.Warnings)
	}
}

func TestProjectProfileUntrustedWorktreeIsIgnored(t *testing.T) {
	result := loadProjectProfileTest(t, `schema_version = 1
[worktree]
copy = [".env"]
setup = "touch should-not-run"
`, "")
	if result.Trusted || !reflect.DeepEqual(result.Worktree, WorktreeSetup{Copy: []string{}, SetupTimeoutSeconds: 600}) {
		t.Fatalf("untrusted worktree result: trusted=%v worktree=%+v", result.Trusted, result.Worktree)
	}
	if !containsString(result.Warnings, "project: [worktree] ignored because the project is not trusted") {
		t.Fatalf("missing untrusted worktree warning: %v", result.Warnings)
	}
}

func TestProjectProfileInvalidChecksRejectTier(t *testing.T) {
	invalid := []struct {
		name string
		body string
	}{
		{"bad_name", "[[checks]]\nname = \"Bad!\"\nargv = [\"python\"]\n"},
		{"duplicate_name", "[[checks]]\nname = \"same\"\nargv = [\"python\"]\n[[checks]]\nname = \"same\"\nargv = [\"python\"]\n"},
		{"empty_argv", "[[checks]]\nname = \"x\"\nargv = []\n"},
		{"non_string_argv", "[[checks]]\nname = \"x\"\nargv = [\"python\", 3]\n"},
		{"zero_timeout", "[[checks]]\nname = \"x\"\nargv = [\"python\"]\ntimeout_seconds = 0\n"},
		{"excessive_timeout", "[[checks]]\nname = \"x\"\nargv = [\"python\"]\ntimeout_seconds = 301\n"},
		{"too_many_checks", manyProjectChecks(9)},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			result := loadProjectProfileTest(t, "schema_version = 1\n"+test.body, "")
			if result.ProjectStatus != "rejected" || len(result.Checks) != 0 || !result.ForceFullReview {
				t.Fatalf("invalid checks result: %+v", result)
			}
		})
	}
}

func TestProjectProfileUserTierChecksRejected(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n", "schema_version = 1\n[[checks]]\nname = \"x\"\nargv = [\"python\"]\n")
	if result.UserStatus != "rejected" || len(result.Checks) != 0 {
		t.Fatalf("user checks result: %+v", result)
	}
}

func TestProjectProfileUnknownTopLevelKeyWarns(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\nunknown_section = true\n", "")
	if !containsString(result.Warnings, "project: unknown top-level key or section [unknown_section] ignored") {
		t.Fatalf("unknown key warning missing: %v", result.Warnings)
	}
}

func TestProjectProfileRoleDefaultsAndReviewerChain(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n", "")
	wantRoles := Roles{Orchestrator: "claude-code", Coder: "codex", Reviewer: "agy"}
	wantCandidates := []ReviewerCandidate{
		{Agent: "agy", Model: "gemini-3.8-flash-high"},
		{Agent: "claude", Model: "claude-haiku-5-5"},
		{Agent: "codex", Model: "gpt-6-luna", Effort: "low"},
	}
	if !reflect.DeepEqual(result.Roles, wantRoles) || !reflect.DeepEqual(result.ReviewerCandidates, wantCandidates) {
		t.Fatalf("defaults: roles=%+v candidates=%+v", result.Roles, result.ReviewerCandidates)
	}
}

func TestProjectProfileReviewerCandidatesLoadInConfiguredOrder(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n", `schema_version = 1
[reviewer.selected]
agent = "agy"
model = "gemini-3.8-flash-high"
[[reviewer.alternatives]]
agent = "codex"
model = "gpt-6-sol"
[[reviewer.alternatives]]
agent = "opencode"
model = "google/gemini-2.5-pro"
`)
	want := []ReviewerCandidate{
		{Agent: "agy", Model: "gemini-3.8-flash-high"},
		{Agent: "codex", Model: "gpt-6-sol"},
		{Agent: "opencode", Model: "google/gemini-2.5-pro"},
	}
	if result.UserStatus != "ok" || result.Roles.Reviewer != "agy" || !reflect.DeepEqual(result.ReviewerCandidates, want) {
		t.Fatalf("configured candidates: status=%q roles=%+v candidates=%+v", result.UserStatus, result.Roles, result.ReviewerCandidates)
	}
}

func TestProjectProfileUserRolesLoadValidValues(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n", "schema_version = 1\n[roles]\norchestrator = \"claude-code\"\ncoder = \"claude\"\nreviewer = \"none\"\n")
	want := Roles{Orchestrator: "claude-code", Coder: "claude", Reviewer: "none"}
	if result.UserStatus != "ok" || !reflect.DeepEqual(result.Roles, want) {
		t.Fatalf("valid user roles: status=%q roles=%+v", result.UserStatus, result.Roles)
	}
}

func TestProjectProfileReviewerClaudeAcceptedByGo(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n", "schema_version = 1\n[roles]\norchestrator = \"claude-code\"\ncoder = \"claude\"\nreviewer = \"claude\"\n")
	want := Roles{Orchestrator: "claude-code", Coder: "claude", Reviewer: "claude"}
	if result.UserStatus != "ok" || !reflect.DeepEqual(result.Roles, want) {
		t.Fatalf("Go-valid claude reviewer role: status=%q roles=%+v", result.UserStatus, result.Roles)
	}
}

func TestProjectProfileInvalidUserRolesRejectTier(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n", "schema_version = 1\n[roles]\nextra = \"agy\"\n")
	want := Roles{Orchestrator: "claude-code", Coder: "codex", Reviewer: "agy"}
	if result.UserStatus != "rejected" || !reflect.DeepEqual(result.Roles, want) || !result.ForceFullReview {
		t.Fatalf("invalid user roles result: %+v", result)
	}
}

func TestProjectProfileProjectRolesDoNotTakeEffect(t *testing.T) {
	result := loadProjectProfileTest(t, "schema_version = 1\n[roles]\nreviewer = \"none\"\n", "")
	if result.ProjectStatus != "ok" || result.Roles.Reviewer != "agy" || !containsString(result.Warnings, "project: unknown top-level key or section [roles] ignored") {
		t.Fatalf("project roles result: %+v", result)
	}
}

func TestProjectProfileProtectedPathCaseRules(t *testing.T) {
	caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	cases := []struct {
		path string
		want bool
	}{
		{".CLAUDE/SETTINGS.JSON", caseInsensitive},
		{".ORCHESTRATOR/PROFILE.TOML", caseInsensitive},
		{".claude/settings.json", true},
		{".orchestrator/profile.toml", true},
	}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			patterns := []string{".claude/settings.json", ".orchestrator/**"}
			if got := IsProtected(test.path, patterns); got != test.want {
				t.Errorf("IsProtected(%q)=%v, want %v (GOOS=%s)", test.path, got, test.want, runtime.GOOS)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func manyProjectChecks(count int) string {
	var checks strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&checks, "[[checks]]\nname = \"c%d\"\nargv = [\"python\"]\n", i)
	}
	return checks.String()
}
