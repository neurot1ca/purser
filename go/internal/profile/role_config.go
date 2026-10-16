package profile

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"purser-hook/internal/proc"
	"purser-hook/internal/projectkey"
)

var projectRoleSections = []string{"roles", "coding", "reviewer", "fallback", "mechanical"}

// ProjectRolesPath returns the user-scoped role file for a repository. Git
// subdirectories share the top-level key; non-git directories use their path.
func ProjectRolesPath(stateRoot, repository string) string {
	root, err := filepath.Abs(repository)
	if err == nil {
		command := proc.Hide(exec.Command("git", "-C", root, "rev-parse", "--show-toplevel"))
		if output, gitErr := command.Output(); gitErr == nil {
			if gitRoot, absErr := filepath.Abs(strings.TrimSpace(string(output))); absErr == nil {
				root = gitRoot
			}
		}
	}
	key := projectkey.For(root)
	return filepath.Join(stateRoot, "projects", key, "roles.toml")
}

// StateRoot returns the active Purser state directory.
func StateRoot() string {
	if root := os.Getenv("PURSER_STATE_DIR"); root != "" {
		return filepath.Clean(root)
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".claude", "purser")
		}
		configDir = filepath.Join(home, ".claude")
	}
	return filepath.Join(configDir, "purser")
}

// ResolveUserConfig overlays the project role sections onto the global user
// configuration. Sections replace as a whole; trust always remains global.
func ResolveUserConfig(stateRoot, repository, globalPath string) (map[string]any, error) {
	global := map[string]any{}
	if globalPath != "" {
		raw, err := os.ReadFile(globalPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			global, _, err = DecodeTOML(raw)
			if err != nil {
				return nil, err
			}
		}
	}
	if repository == "" {
		return global, nil
	}
	projectRaw, err := os.ReadFile(ProjectRolesPath(stateRoot, repository))
	if errors.Is(err, os.ErrNotExist) {
		return global, nil
	}
	if err != nil {
		return nil, err
	}
	project, _, err := DecodeTOML(projectRaw)
	if err != nil {
		return nil, err
	}
	if !isSchemaVersionOne(project["schema_version"]) {
		return nil, errors.New("project roles schema_version must be 1")
	}
	allowed := map[string]bool{"schema_version": true, "trust": true}
	for _, section := range projectRoleSections {
		allowed[section] = true
	}
	for name, value := range project {
		if !allowed[name] {
			return nil, fmt.Errorf("unknown project roles section [%s]", name)
		}
		if name != "schema_version" && name != "trust" {
			if _, ok := value.(map[string]any); !ok {
				return nil, fmt.Errorf("project roles [%s] must be a table", name)
			}
		}
	}
	if _, exists := global["schema_version"]; !exists {
		if value, present := project["schema_version"]; present {
			global["schema_version"] = value
		}
	}
	for _, section := range projectRoleSections {
		if value, exists := project[section]; exists {
			global[section] = value
		}
	}
	return global, nil
}
