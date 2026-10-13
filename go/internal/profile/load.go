package profile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

// LoadProfile reads the default user profile and the project profile, then
// resolves them using the same trust and precedence rules as the Python hook.
// An optional userConfigPath overrides ~/.orchestrator/config.toml.
func LoadProfile(projectRoot string, userConfigPath ...string) (resolved ResolvedProfile) {
	defer func() {
		if failure := recover(); failure != nil {
			resolved = failedProfile(panicText(failure))
		}
	}()
	userPath := ""
	if len(userConfigPath) > 0 {
		userPath = userConfigPath[0]
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return failedProfile(errorText(err))
		}
		userPath = filepath.Join(home, ".orchestrator", "config.toml")
	}
	return loadProfileFiles(projectRoot, userPath, os.ReadFile)
}

func loadProfileFiles(projectRoot, userPath string, readFile func(string) ([]byte, error)) (resolved ResolvedProfile) {
	defer func() {
		if failure := recover(); failure != nil {
			resolved = failedProfile(panicText(failure))
		}
	}()
	user := loadTierFile(userPath, "user", readFile)
	rolePath := ProjectRolesPath(StateRoot(), projectRoot)
	if _, err := os.Stat(rolePath); !errors.Is(err, os.ErrNotExist) {
		config, resolveErr := ResolveUserConfig(StateRoot(), projectRoot, userPath)
		if resolveErr != nil {
			user = rejectedFileTier("user", resolveErr)
		} else {
			user = ValidateTier("user", config)
		}
	}
	project := loadTierFile(filepath.Join(projectRoot, ".orchestrator", "profile.toml"), "project", readFile)
	return ResolveProfile(projectRoot, user, project)
}

func loadTierFile(path, tier string, readFile func(string) ([]byte, error)) Tier {
	raw, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Tier{Status: "missing", Values: map[string]any{}, Warnings: []string{}}
	}
	if err != nil {
		return rejectedFileTier(tier, err)
	}
	return ValidateTierTOML(tier, raw)
}

func rejectedFileTier(tier string, err error) Tier {
	return Tier{Status: "rejected", Values: map[string]any{}, Warnings: []string{
		tier + " tier rejected: could not parse profile: " + errorText(err),
	}}
}

func errorText(err error) string {
	if text := err.Error(); text != "" {
		return text
	}
	return fmt.Sprintf("%T", err)
}

func panicText(value any) string {
	if err, ok := value.(error); ok {
		return errorText(err)
	}
	return fmt.Sprint(value)
}

func failedProfile(reason string) ResolvedProfile {
	protected := append([]string(nil), engineProtected...)
	sort.Strings(protected)
	return ResolvedProfile{
		Configured: false, ProtectedPaths: protected,
		ForceFullReview: true, SkipMaxLines: 50, SkipMaxFiles: 2,
		CoverageTargetPct:  90,
		ReviewerCandidates: []ReviewerCandidate{{Agent: "agy", Model: "gemini-3.8-flash-high"}},
		Roles:              Roles{Orchestrator: "claude-code", Coder: "codex", Reviewer: "agy"},
		AuthoritySources:   DefaultAuthoritySources(),
		Checks:             []Check{},
		Worktree:           WorktreeSetup{Copy: []string{}, SetupTimeoutSeconds: 600},
		UserStatus:         "rejected", ProjectStatus: "rejected",
		Warnings: []string{"profile load failed: " + reason},
	}
}

// DecodeTOML returns the generic table and source-ordered key paths used by tier validation.
func DecodeTOML(raw []byte) (map[string]any, []toml.Key, error) {
	data := map[string]any{}
	metadata, err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&data)
	if err != nil {
		return nil, nil, err
	}
	return normalizeDecodedValue(data).(map[string]any), metadata.Keys(), nil
}

// ValidateTierTOML decodes and validates a raw profile while preserving source order.
func ValidateTierTOML(tier string, raw []byte) Tier {
	data, keys, err := DecodeTOML(raw)
	if err != nil {
		return Tier{Status: "rejected", Values: map[string]any{}, Warnings: []string{
			tier + " tier rejected: could not parse profile: " + err.Error(),
		}}
	}
	return ValidateTier(tier, data, keys)
}

func normalizeDecodedValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			value[key] = normalizeDecodedValue(item)
		}
		return value
	case []map[string]any:
		items := make([]any, len(value))
		for i, item := range value {
			items[i] = normalizeDecodedValue(item)
		}
		return items
	case []any:
		for i, item := range value {
			value[i] = normalizeDecodedValue(item)
		}
		return value
	default:
		return value
	}
}
