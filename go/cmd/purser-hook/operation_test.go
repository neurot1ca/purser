package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/jobs"
	"purser-hook/internal/testutil"
	"runtime"
	"strings"
	"testing"
	"time"
)

func buildPurserHook(t *testing.T) string {
	t.Helper()
	name := "purser-hook"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	command := testutil.Hide(exec.Command("go", "build", "-o", path, "."))
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
	command := testutil.Hide(exec.Command(binary, argv...))
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

func TestCodexStartOpenCodeIgnoresEmptyCodexOverride(t *testing.T) {
	t.Setenv("PURSER_AGENT", "")
	t.Setenv("PURSER_CODEX_BIN", "")
	fakeDir := t.TempDir()
	fakeSource := filepath.Join(fakeDir, "main.go")
	if err := os.WriteFile(fakeSource, []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeAgent := filepath.Join(fakeDir, "opencode")
	if runtime.GOOS == "windows" {
		fakeAgent += ".exe"
	}
	build := testutil.Hide(exec.Command("go", "build", "-o", fakeAgent, fakeSource))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake OpenCode: %v\n%s", err, output)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := t.TempDir()
	call := "123e4567-e89b-12d3-a456-426614174004"
	args := map[string]any{
		"agent": "opencode", "call": call, "config": map[string]any{}, "cwd": t.TempDir(),
		"model": "provider/model", "prompt": "test",
	}
	override := os.Getenv("PURSER_CODEX_BIN")
	var codexBin any
	if override != "" {
		codexBin = override
	}
	result, _, err := jobs.Start(state, args, jobs.StartOptions{
		CodexBin: codexBin,
		Validation: jobs.StartValidationOptions{
			Agent: os.Getenv("PURSER_AGENT"), FixedAgent: os.Getenv("PURSER_AGENT") != "",
			ValidateAgent: func(agent string) error {
				if agent == "codex" {
					_, err := jobs.ResolveCodexPrefix(override)
					return err
				}
				_, err := jobs.ResolveAgentPrefix(agent, nil)
				return err
			},
		},
		Launch: func(string, int64, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	id := sha256.Sum256([]byte(call))
	jobID := hex.EncodeToString(id[:])
	structured, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("start result missing structured content: %#v", result)
	}
	if structured["errorCode"] == "INVALID_ARGUMENT" {
		t.Fatalf("OpenCode start was rejected as an invalid argument: %#v", result)
	}
	launch, err := os.ReadFile(filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".launch"))
	if err != nil {
		t.Fatalf("OpenCode launch record unavailable: result=%#v err=%v", result, err)
	}
	var recorded map[string]any
	if err = json.Unmarshal(launch, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded["agent"] != "opencode" || recorded["codexBin"] != nil {
		t.Fatalf("Codex override leaked into OpenCode launch: %s", launch)
	}
}

func TestCodexStartUsesConfiguredAgentAndBinaryOverride(t *testing.T) {
	binary := buildPurserHook(t)
	plugin := t.TempDir()
	configDir := filepath.Join(plugin, ".claude", "hooks", "orchestrator")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(configDir, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	userConfig := filepath.Join(home, ".orchestrator")
	if err = os.MkdirAll(userConfig, 0700); err != nil {
		t.Fatal(err)
	}
	configured := "[coding]\ncoder_agent = \"agy\"\nfamily = \"team\"\n[coding.routes]\nmechanical = { model = \"gemini-test\", effort = \"high\" }\nsubtle = { model = \"gemini-test\", effort = \"high\" }\nnormal = { model = \"gemini-test\", effort = \"high\" }\nconsequential = { model = \"gemini-test\", effort = \"high\" }\nescalated = { model = \"gemini-test\", effort = \"high\" }\noperation = { model = \"gemini-test\", effort = \"high\" }\nplan_standard = { model = \"gemini-test\", effort = \"high\" }\n[roles]\ncoder = \"agy\"\n"
	if err = os.WriteFile(filepath.Join(userConfig, "config.toml"), []byte(configured), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("PURSER_AGENT", "")
	state := t.TempDir()
	fakeDir := t.TempDir()
	fakeSource := filepath.Join(fakeDir, "main.go")
	if err = os.WriteFile(fakeSource, []byte("package main\nimport \"time\"\nfunc main() { time.Sleep(time.Second) }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(fakeDir, "fake-codex")
	if runtime.GOOS == "windows" {
		override += ".exe"
	}
	build := testutil.Hide(exec.Command("go", "build", "-o", override, fakeSource))
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build fake worker: %v\n%s", buildErr, output)
	}
	t.Setenv("PURSER_CODEX_BIN", override)
	args := map[string]any{"call": "123e4567-e89b-12d3-a456-426614174001", "prompt": "test", "cwd": t.TempDir(),
		"model": "gemini-test", "config": map[string]any{"model_reasoning_effort": "high"}}
	input, _ := json.Marshal(map[string]any{"args": args})
	code, output := runPrivate(t, binary, state, "op codex_start", string(input))
	if code != 0 {
		t.Fatalf("start exit=%d output=%s", code, output)
	}
	id := sha256.Sum256([]byte(args["call"].(string)))
	launch, err := os.ReadFile(filepath.Join(state, "codex_exec_mcp", "jobs", hex.EncodeToString(id[:])+".launch"))
	if err != nil {
		t.Fatalf("launch record unavailable: %v; operation output=%s", err, output)
	}
	var recorded map[string]any
	if err = json.Unmarshal(launch, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded["agent"] != "agy" || recorded["codexBin"] != nil {
		t.Fatalf("Codex executable override leaked into configured agent launch: %s", launch)
	}
	t.Setenv("PURSER_AGENT", "codex")
	args["call"] = "123e4567-e89b-12d3-a456-426614174003"
	input, _ = json.Marshal(map[string]any{"args": args})
	code, output = runPrivate(t, binary, state, "op codex_start", string(input))
	if code != 0 {
		t.Fatalf("environment-selected start exit=%d output=%s", code, output)
	}
	id = sha256.Sum256([]byte(args["call"].(string)))
	launch, err = os.ReadFile(filepath.Join(state, "codex_exec_mcp", "jobs", hex.EncodeToString(id[:])+".launch"))
	if err != nil {
		t.Fatalf("environment-selected launch record unavailable: %v; operation output=%s", err, output)
	}
	if err = json.Unmarshal(launch, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded["agent"] != "codex" || recorded["codexBin"] != override {
		t.Fatalf("PURSER_AGENT did not select Codex with its executable override: %s", launch)
	}
	t.Setenv("PURSER_AGENT", "codex")
	t.Setenv("PURSER_CODEX_BIN", "[]")
	args["call"] = "123e4567-e89b-12d3-a456-426614174002"
	input, _ = json.Marshal(map[string]any{"args": args})
	code, output = runPrivate(t, binary, state, "op codex_start", string(input))
	if code != 0 {
		t.Fatalf("invalid launch exit=%d output=%s", code, output)
	}
	failed := decodeOutput(t, output)["result"].(map[string]any)["structuredContent"].(map[string]any)
	if failed["state"] != "FAILED" || failed["errorCode"] != "INVALID_ARGUMENT" {
		t.Fatalf("invalid executable override was not rejected before launch: %#v", failed)
	}
	for _, call := range []string{"123e4567-e89b-12d3-a456-426614174001", "123e4567-e89b-12d3-a456-426614174003"} {
		id := sha256.Sum256([]byte(call))
		path := filepath.Join(state, "codex_exec_mcp", "jobs", hex.EncodeToString(id[:])+".json")
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			data, readErr := os.ReadFile(path)
			var job map[string]any
			if readErr == nil && json.Unmarshal(data, &job) == nil &&
				(job["state"] == "SUCCEEDED" || job["state"] == "FAILED") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
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
	command := testutil.Hide(exec.Command(binary, "host", dir, jobID, "1", token))
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
