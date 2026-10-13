package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"purser-hook/internal/contextio"
	"purser-hook/internal/profile"
)

func TestCoderEffortRejectsBadValue(t *testing.T) {
	if code := coderEffortMain([]string{"ultra"}); code != 2 {
		t.Fatalf("bad effort exit code = %d, want 2", code)
	}
}

func TestCoderEffortUsesProjectCoderRole(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	global := filepath.Join(work, "home", ".orchestrator", "config.toml")
	if err := os.MkdirAll(filepath.Dir(global), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[roles]\ncoder = \"codex\"\n[coding]\ncoder_agent = \"codex\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(work, "state")
	t.Setenv("CLAUDE_PLUGIN_ROOT", root)
	t.Setenv("USERPROFILE", filepath.Join(work, "home"))
	t.Setenv("PURSER_STATE_DIR", state)

	claudeRepo := filepath.Join(work, "claude")
	codexRepo := filepath.Join(work, "codex")
	for _, repository := range []string{claudeRepo, codexRepo} {
		if err := os.MkdirAll(filepath.Join(repository, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	claudeRoles := profile.ProjectRolesPath(state, claudeRepo)
	if err := os.MkdirAll(filepath.Dir(claudeRoles), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeRoles, []byte("schema_version = 1\n[roles]\ncoder = \"claude\"\n[coding]\ncoder_agent = \"claude\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := gearConfig(claudeRepo)
	if err != nil || config["coder_agent"] != "claude" {
		t.Fatalf("project coder config = %#v, error = %v; roles path %q", config, err, claudeRoles)
	}
	if _, _, code := runCoderEffortForTest(t, "high", "--cwd", claudeRepo); code == 0 {
		t.Fatal("project Claude coder accepted Codex effort")
	}
	codexRoles := profile.ProjectRolesPath(state, codexRepo)
	if err := os.MkdirAll(filepath.Dir(codexRoles), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexRoles, []byte("schema_version = 1\n[roles]\ncoder = \"codex\"\n[coding]\ncoder_agent = \"codex\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("[roles]\ncoder = \"claude\"\n[coding]\ncoder_agent = \"claude\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runCoderEffortForTest(t, "high", "--cwd", codexRepo); code != 0 {
		t.Fatal("project Codex coder rejected Codex effort over global Claude role")
	}
}

func TestProjectRoleGearOutputUsesProjectRoutes(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	repository := filepath.Join(work, "repo")
	if err := os.MkdirAll(repository, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(work, "state")
	roles := profile.ProjectRolesPath(state, repository)
	if err := os.MkdirAll(filepath.Dir(roles), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(roles, []byte("schema_version = 1\n[roles]\ncoder = \"claude\"\n[coding]\ncoder_agent = \"claude\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", root)
	t.Setenv("USERPROFILE", filepath.Join(work, "home"))
	t.Setenv("PURSER_STATE_DIR", state)
	stdout, stderr, code := runGearForTest(t, "up", "--cwd", repository)
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout), &output); err != nil || code != 0 || stderr != "" {
		t.Fatalf("gear result = (%q, %q, %d), parse error %v", stdout, stderr, code, err)
	}
	routes, ok := output["routes"].(map[string]any)
	if !ok || len(routes) != 0 {
		t.Fatalf("gear routes = %#v, want empty routes for the project's Claude coder", output["routes"])
	}
}

func runGearForTest(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	code := gearMain(args)
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	stdout, readErr := io.ReadAll(outR)
	_ = outR.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	stderr, readErr := io.ReadAll(errR)
	_ = errR.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(stdout), string(stderr), code
}

func TestCoderEffortNoArgumentReadsCurrentSettingWithoutWriting(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	repository := filepath.Join(work, "repo")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(work, "state")
	t.Setenv("CLAUDE_PLUGIN_ROOT", root)
	t.Setenv("USERPROFILE", filepath.Join(work, "home"))
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	key := contextio.ProjectKey(repository)
	if err := contextio.SetCoderEffort(stateRoot, key, "high"); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateRoot, "projects", key, "coder-effort.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCoderEffortForTest(t, "--cwd", repository)
	if code != 0 || stdout != "{\"applies_to\":\"new Codex jobs\",\"coder_effort\":\"high\"}\n" || stderr != "" {
		t.Fatalf("read result = (%q, %q, %d)", stdout, stderr, code)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("coder effort state changed: before %q, after %q", before, after)
	}
}

func runCoderEffortForTest(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	code := coderEffortMain(args)
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	stdout, readErr := io.ReadAll(outR)
	_ = outR.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	stderr, readErr := io.ReadAll(errR)
	_ = errR.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(stdout), string(stderr), code
}
