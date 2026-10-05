package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func buildPurserHook(t *testing.T) string {
	t.Helper()
	name := "purser-hook"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	command := exec.Command("go", "build", "-o", path, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build purser-hook: %v\n%s", err, output)
	}
	return path
}

func runPrivate(t *testing.T, binary, state, operation, input string) (int, []byte) {
	t.Helper()
	argv := []string{}
	if operation != "" {
		argv = strings.Fields(operation)
	}
	command := exec.Command(binary, argv...)
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "PURSER_STATE_DIR="+state)
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err == nil {
		return 0, output
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), output
	}
	t.Fatalf("run %s: %v", operation, err)
	return -1, nil
}

func decodeOutput(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("invalid JSON output %q: %v", data, err)
	}
	return result
}

func TestPrivateOperationLifecycleAndHookOutput(t *testing.T) {
	binary := buildPurserHook(t)
	state := t.TempDir()
	fakeWorker := filepath.Join(t.TempDir(), "fake-worker")
	if runtime.GOOS == "windows" {
		fakeWorker += ".exe"
	}
	if err := os.WriteFile(fakeWorker, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_CODEX_BIN", fakeWorker)
	startArgs := map[string]any{"call": "123e4567-e89b-12d3-a456-426614174000", "prompt": "test", "cwd": t.TempDir()}
	startJSON, _ := json.Marshal(map[string]any{"args": startArgs})
	code, output := runPrivate(t, binary, state, "op codex_start", string(startJSON))
	if code != 0 {
		t.Fatalf("start exit=%d output=%s", code, output)
	}
	started := decodeOutput(t, output)
	if started["status"] != "ok" && started["status"] != "error" {
		t.Fatalf("unexpected start status: %#v", started)
	}
	sum := sha256.Sum256([]byte(startArgs["call"].(string)))
	jobID := hex.EncodeToString(sum[:])
	resultInput, _ := json.Marshal(map[string]any{"args": map[string]any{"jobId": jobID}})
	code, output = runPrivate(t, binary, state, "op codex_result", string(resultInput))
	if code != 0 || decodeOutput(t, output)["status"] == nil {
		t.Fatalf("result exit=%d output=%s", code, output)
	}
	cancelInput, _ := json.Marshal(map[string]any{"args": map[string]any{"jobId": jobID}})
	code, output = runPrivate(t, binary, state, "op codex_cancel", string(cancelInput))
	if code != 0 || decodeOutput(t, output)["status"] == nil {
		t.Fatalf("cancel exit=%d output=%s", code, output)
	}
	applyInput, _ := json.Marshal(map[string]any{"args": map[string]any{"jobId": strings.Repeat("f", 64)}})
	code, output = runPrivate(t, binary, state, "op codex_apply", string(applyInput))
	if code != 0 || decodeOutput(t, output)["status"] != "error" {
		t.Fatalf("apply failure status missing: exit=%d output=%s", code, output)
	}

	code, output = runPrivate(t, binary, state, "", `{"hook_event_name":"PostToolUseFailure","tool_name":"Bash"}`)
	if code != 0 || len(output) != 0 {
		t.Fatalf("hook output changed: exit=%d output=%q", code, output)
	}
}

func TestPrivateOperationRejectsUnknownAndMalformedInput(t *testing.T) {
	binary := buildPurserHook(t)
	state := t.TempDir()
	code, output := runPrivate(t, binary, state, "op not_a_tool", `{"args":{}}`)
	if code == 0 || decodeOutput(t, output)["status"] != "error" {
		t.Fatalf("unknown op was not explicit: exit=%d output=%s", code, output)
	}
	code, output = runPrivate(t, binary, state, "op codex_start", `{`)
	if code == 0 || decodeOutput(t, output)["status"] != "error" {
		t.Fatalf("malformed args were not explicit: exit=%d output=%s", code, output)
	}
}

func TestHostEntryPointReportsExplicitErrorStatus(t *testing.T) {
	binary := buildPurserHook(t)
	state := t.TempDir()
	dir := filepath.Join(state, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	jobID := strings.Repeat("a", 64)
	token := "test-token"
	job := map[string]any{"jobId": jobID, "state": "QUEUED", "generation": 1, "ownershipToken": token}
	data, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	launch, _ := json.Marshal(map[string]any{"name": "codex", "agent": "opencode", "args": map[string]any{}})
	if err := os.WriteFile(filepath.Join(dir, jobID+".launch"), launch, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "host", dir, jobID, "1", token)
	command.Env = append(os.Environ(), "PURSER_STATE_DIR="+state)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("host unexpectedly returned success: %s", output)
	}
	var status map[string]any
	if json.Unmarshal(output, &status) != nil || status["status"] != "error" {
		t.Fatalf("host error status missing: %q", output)
	}
}
