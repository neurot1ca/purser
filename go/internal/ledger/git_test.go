package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGitPythonFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "git.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Scope  []string            `json:"scope"`
		Status []map[string]string `json:"status"`
		Index  map[string][]string `json:"index"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	root := initGitRepository(t)
	if err = os.MkdirAll(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"dir/file[1].txt": "literal", "dir/file1.txt": "wildcard match", "old.txt": "rename me\n"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err = os.WriteFile(path, []byte(content), 0666); err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(path, 0666); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", "--all")
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	if err = os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(root, "dir/file[1].txt")
	if err = os.WriteFile(changed, []byte("changed"), 0666); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(changed, 0666); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "-A")
	status, err := statusEntries(root, time.Second)
	if err != nil || !reflect.DeepEqual(status, fixture.Status) {
		t.Fatalf("status = %#v, %v; want %#v", status, err, fixture.Status)
	}
	scope, err := normalizeScope([]string{" dir\\file[1].txt ", "old.txt", "renamed.txt", "dir/file[1].txt"})
	if err != nil || !reflect.DeepEqual(scope, fixture.Scope) {
		t.Fatalf("scope = %#v, %v; want %#v", scope, err, fixture.Scope)
	}
	index, err := indexEntries(root, scope, time.Second)
	if err != nil || !reflect.DeepEqual(index, fixture.Index) {
		t.Fatalf("index = %#v, %v; want %#v", index, err, fixture.Index)
	}
}

func TestGitMalformedOutput(t *testing.T) {
	for _, raw := range [][]byte{[]byte("bad\x00"), []byte("R  new\x00")} {
		_, err := parseStatus(raw)
		failure, ok := err.(*inspectionError)
		if !ok || failure.Code != "GIT_OUTPUT_INVALID" {
			t.Fatalf("malformed status = %v", err)
		}
	}
	failure := &inspectionError{"GIT_OUTPUT_INVALID", "Git status output was malformed."}
	encoded, _ := json.Marshal(map[string]string{"code": failure.Code, "message": failure.Message})
	fmt.Println(string(encoded))
}

func TestGitFailureIncludesBoundedStderr(t *testing.T) {
	root := t.TempDir()
	_, err := runGit(root, []string{"apply", "--check", "-"}, time.Second,
		"checking patch", []byte("not a patch\n"), nil)
	failure, ok := err.(*inspectionError)
	if !ok || failure.Code != "GIT_FAILED" || !strings.Contains(failure.Message, "Git stderr:") ||
		!strings.Contains(failure.Message, "No valid patches") {
		t.Fatalf("Git failure did not include stderr: %v", err)
	}
	if got := gitStderrSummary(strings.Repeat("x", 500)); len(got) != 400 {
		t.Fatalf("stderr summary length = %d, want 400", len(got))
	}
}

func TestGitLiveOutcome(t *testing.T) {
	if os.Getenv("PURSER_GIT_LIVE_OUTCOME") == "" {
		t.Skip("set PURSER_GIT_LIVE_OUTCOME to enable the live outcome check")
	}
	root := os.Getenv("PURSER_GIT_LIVE_ROOT")
	mode := os.Getenv("PURSER_GIT_LIVE_OUTCOME")
	var env map[string]string
	timeout := time.Second
	if mode == "timeout" {
		timeout = 0
	}
	if mode == "unavailable" {
		env = map[string]string{"PATH": os.Getenv("PURSER_GIT_LIVE_PATH")}
	}
	if mode == "failed" {
		env = map[string]string{"GIT_CEILING_DIRECTORIES": os.Getenv("PURSER_GIT_LIVE_CEILING")}
	}
	_, err := runGit(root, []string{"status"}, timeout, "reading repository status", nil, env)
	var result map[string]string
	if err == nil {
		result = map[string]string{"code": "OK", "message": ""}
	} else {
		failure, ok := err.(*inspectionError)
		if !ok {
			t.Fatalf("unexpected outcome %T: %v", err, err)
		}
		result = map[string]string{"code": failure.Code, "message": failure.Message}
	}
	encoded, _ := json.Marshal(result)
	_, _ = os.Stdout.Write(append(encoded, '\n'))
}

func initGitRepository(t *testing.T) string {
	t.Helper()
	root := gitTempDir(t)
	git(t, root, "init", "-q")
	return root
}

func gitTempDir(t *testing.T) string {
	t.Helper()
	parent := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, "go-ledger-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
