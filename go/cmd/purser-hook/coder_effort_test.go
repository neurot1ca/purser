package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"purser-hook/internal/contextio"
)

func TestCoderEffortRejectsBadValue(t *testing.T) {
	if code := coderEffortMain([]string{"ultra"}); code != 2 {
		t.Fatalf("bad effort exit code = %d, want 2", code)
	}
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
