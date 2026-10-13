package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectRoleResolution(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	global := filepath.Join(base, "global.toml")
	if err := os.WriteFile(global, []byte("[roles]\ncoder = \"codex\"\n[mechanical]\nopencode_enabled = false\n[trust]\npaths = [\"/trusted\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	projectA := filepath.Join(base, "a")
	projectB := filepath.Join(base, "b")
	if err := os.MkdirAll(projectA, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectB, 0700); err != nil {
		t.Fatal(err)
	}
	rolePath := ProjectRolesPath(state, projectA)
	if err := os.MkdirAll(filepath.Dir(rolePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolePath, []byte("schema_version = 1\n[roles]\ncoder = \"claude\"\n[trust]\npaths = [\"/project-granted\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := ResolveUserConfig(state, projectA, global)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveUserConfig(state, projectB, global)
	if err != nil {
		t.Fatal(err)
	}
	if a["roles"].(map[string]any)["coder"] != "claude" || b["roles"].(map[string]any)["coder"] != "codex" {
		t.Fatalf("project isolation failed: A=%#v B=%#v", a["roles"], b["roles"])
	}
	if a["mechanical"].(map[string]any)["opencode_enabled"] != false || a["trust"].(map[string]any)["paths"].([]any)[0] != "/trusted" {
		t.Fatalf("missing-section fallback or global trust failed: %#v", a)
	}
	if _, ok := b["trust"].(map[string]any); !ok {
		t.Fatalf("global trust missing: %#v", b)
	}

	if err := os.WriteFile(rolePath, []byte("[roles\ncoder = \"claude\""), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveUserConfig(state, projectA, global); err == nil {
		t.Fatal("malformed project roles silently fell back")
	}
}

func TestProjectRoleClaudeCodingAcceptedByProfileTier(t *testing.T) {
	got := ValidateTier("user", map[string]any{"schema_version": 1, "coding": map[string]any{
		"coder_agent": "claude", "reviewer1_agent": "claude",
		"reviewer1_model_requested": "claude-sonnet-4-5", "reviewer1_effort": "high",
	}})
	if got.Status != "ok" {
		t.Fatalf("Claude coding role rejected: %+v", got)
	}
}

func TestProjectRoleLoadProfile(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("PURSER_STATE_DIR", state)
	userPath := filepath.Join(root, "global.toml")
	if err := os.WriteFile(userPath, []byte("schema_version = 1\n[roles]\ncoder = \"codex\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rolePath := ProjectRolesPath(state, root)
	if err := os.MkdirAll(filepath.Dir(rolePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rolePath, []byte("schema_version = 1\n[roles]\ncoder = \"claude\"\n[coding]\ncoder_agent = \"claude\"\nreviewer1_agent = \"claude\"\nreviewer1_model_requested = \"claude-sonnet-4-5\"\nreviewer1_effort = \"high\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := LoadProfile(root, userPath)
	if got.UserStatus != "ok" || got.Roles.Coder != "claude" {
		t.Fatalf("profile did not resolve project roles: status=%q roles=%+v warnings=%v", got.UserStatus, got.Roles, got.Warnings)
	}
}

func TestProjectRoleNonGitPathUsesAbsoluteDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute))))
	want := filepath.Join("state", "projects", hex.EncodeToString(key[:])[:16], "roles.toml")
	if got := ProjectRolesPath("state", root); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}
