package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func nativeArgs(root, executable, _ string) map[string]any {
	repo := filepath.Join(root, "repo")
	return map[string]any{
		"task_id": "native-test", "phase": "implementation", "route": "normal", "model": "gpt-6-sol", "effort": "medium",
		"repo_root": repo, "delta": map[string]any{"changed_paths": []any{"change.txt"}, "baseline_head": nativeHead(repo), "ambiguous_paths": []any{}, "changed_file_count": 1, "changed_line_count": 1, "sources": map[string]any{"scoped_manifest": map[string]any{"paths": []any{}}}},
		"classification": nil, "task_objective": "Check native review", "reviewer_executable": executable,
		"reviewer_agent": "agy", "reviewer_model": pinnedModel, "reviewer_timeout_seconds": 60.0,
	}
}

func isolateReviewerPath(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
}

func nativeHead(root string) string {
	output, _ := testutil.Hide(exec.Command("git", "-C", root, "rev-parse", "HEAD")).Output()
	return strings.TrimSpace(string(output))
}

func nativeFixture(t *testing.T) (string, string) {
	t.Helper()
	scratch := filepath.Join("..", "..", "..", ".tmp")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(scratch, "native-review-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("review me\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Native test"}, {"add", "change.txt"}, {"commit", "-qm", "baseline"}} {
		cmd := testutil.Hide(exec.Command("git", append([]string{"-C", repo}, args...)...))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("review me, changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := `package main
import ("fmt"; "os"; "strings"; "time")
func main() { if d := os.Getenv("MOCK_SLEEP_MS"); d != "" { var n int; fmt.Sscanf(d, "%d", &n); time.Sleep(time.Duration(n)*time.Millisecond) }; output := os.Getenv("MOCK_OUTPUT"); if d := os.Getenv("MOCK_OUTPUT_SIZE"); d != "" { var n int; fmt.Sscanf(d, "%d", &n); output = strings.Repeat("x", n) }; fmt.Print(output); if d := os.Getenv("MOCK_EXIT_CODE"); d != "" { var n int; fmt.Sscanf(d, "%d", &n); os.Exit(n) } }`
	src := filepath.Join(dir, "mock.go")
	if err := os.WriteFile(src, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "agy")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := testutil.Hide(exec.Command("go", "build", "-o", bin, src)).CombinedOutput(); err != nil {
		t.Fatalf("build mock reviewer: %v: %s", err, out)
	}
	config := map[string]any{"reviewer1_model_requested": pinnedModel, "reviewer1_timeout_seconds": 60, "reviewer1_packet_max_bytes": 200000}
	encoded, _ := json.Marshal(config)
	torch := filepath.Join(dir, ".claude", "hooks", "orchestrator")
	if err := os.MkdirAll(torch, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(torch, "config.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, bin
}

func envelope(result string, findings []map[string]string) string {
	b, _ := json.Marshal(map[string]any{"conversation_id": "mock-1", "status": "success", "response": "", "duration_seconds": 0.01, "num_turns": 1, "usage": map[string]any{}, "structured_output": map[string]any{"result": result, "validation": "PASS", "findings": findings}})
	return string(b)
}

func TestNativeRunBridgeCompletedPassAndFindings(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		findings     []map[string]string
	}{
		{"pass", NoMaterialFinding, []map[string]string{}},
		{"findings", ChangesRequired, []map[string]string{{"code": "BUG", "message": "bad"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, bin := nativeFixture(t)
			args := nativeArgs(root, bin, tc.result)
			isolateReviewerPath(t)
			t.Setenv("MOCK_OUTPUT", envelope(tc.result, tc.findings))
			got, err := RunBridge(context.Background(), root, args, 30*time.Second)
			if err != nil || got.Outcome != "completed" || got.Attributes["result"] != tc.result {
				t.Fatalf("RunBridge = %#v, %v", got, err)
			}
		})
	}
}

func TestNativeRunBridgeUnavailableAndDeadline(t *testing.T) {
	root, bin := nativeFixture(t)
	missing := filepath.Join(root, "missing", "agy")
	if runtime.GOOS == "windows" {
		missing += ".exe"
	}
	args := nativeArgs(root, missing, NoMaterialFinding)
	isolateReviewerPath(t)
	got, err := RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Outcome != "unavailable" || got.Attributes["error_code"] != "REVIEWER_EXECUTABLE_MISSING" {
		t.Fatalf("missing executable = %#v, %v", got, err)
	}
	t.Setenv("MOCK_SLEEP_MS", "500")
	t.Setenv("MOCK_OUTPUT", envelope(NoMaterialFinding, []map[string]string{}))
	args["reviewer_executable"] = bin
	args["reviewer_timeout_seconds"] = 0.08
	started := time.Now()
	got, err = RunBridge(context.Background(), root, args, 300*time.Millisecond)
	elapsed := time.Since(started)
	if err != nil || got.Outcome != "unavailable" || got.Attributes["error_code"] != "REVIEWER_TIMEOUT" || elapsed > 400*time.Millisecond {
		t.Fatalf("timeout = %#v, %v, elapsed %s", got, err, elapsed)
	}
}

func TestNativeRunBridgeInvalidResponseAndIncompletePacket(t *testing.T) {
	root, bin := nativeFixture(t)
	t.Setenv("MOCK_OUTPUT", `{}`)
	args := nativeArgs(root, bin, NoMaterialFinding)
	isolateReviewerPath(t)
	got, err := RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Attributes["error_code"] != ResponseInvalid {
		t.Fatalf("invalid response = %#v, %v", got, err)
	}
	args["delta"] = map[string]any{}
	got, err = RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Outcome != "incomplete" || got.Attributes["review_status"] != "NOT_RUN_INCOMPLETE_PACKET" {
		t.Fatalf("incomplete packet = %#v, %v", got, err)
	}
}

func TestNativeRunBridgeInvalidInputAndReviewerPanic(t *testing.T) {
	root, bin := nativeFixture(t)
	args := nativeArgs(root, bin, NoMaterialFinding)
	panicArgs := nativeArgs(root, bin, NoMaterialFinding)
	isolateReviewerPath(t)
	args["delta"] = make(chan int)
	_, err := RunBridge(context.Background(), root, args, 30*time.Second)
	var bridgeErr *BridgeError
	if !errors.As(err, &bridgeErr) || bridgeErr.Code != "INVALID_INPUT" {
		t.Fatalf("invalid input = %v", err)
	}
	registerPanicAdapter(t)
	panicArgs["reviewer_agent"] = "panic-test"
	got, err := RunBridge(context.Background(), root, panicArgs, 30*time.Second)
	if !IsReviewerBridgeException(err) || got.Attributes["error_code"] != "REVIEWER_BRIDGE_EXCEPTION" || !strings.Contains(got.Attributes["findings"].([]map[string]string)[0]["message"], "(string)") {
		t.Fatalf("panic = %#v, %v", got, err)
	}
}

func TestNativeRunBridgeRejectsOversizedReviewerResponse(t *testing.T) {
	root, bin := nativeFixture(t)
	args := nativeArgs(root, bin, NoMaterialFinding)
	isolateReviewerPath(t)
	t.Setenv("MOCK_OUTPUT_SIZE", strconv.Itoa(maxReviewerStdoutBytes+1))
	got, err := RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Outcome != "unavailable" || got.Attributes["error_code"] != ResponseInvalid {
		t.Fatalf("oversized response = %#v, %v", got, err)
	}
}

func TestNativeReviewerDiagnosticRecordsParseFailure(t *testing.T) {
	root, bin := nativeFixture(t)
	claude := filepath.Join(root, "claude"+filepath.Ext(bin))
	if err := os.Rename(bin, claude); err != nil {
		t.Fatal(err)
	}
	bin = claude
	state := filepath.Join(root, "state")
	t.Setenv("PURSER_STATE_DIR", state)
	stdout := "invalid-reviewer-response"
	t.Setenv("MOCK_OUTPUT", stdout)
	args := nativeArgs(root, bin, NoMaterialFinding)
	args["reviewer_agent"] = "claude"
	args["reviewer_model"] = "claude-sonnet-4-5"
	args["reviewer_effort"] = "high"
	isolateReviewerPath(t)
	got, err := RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Attributes["error_code"] != ResponseInvalid {
		t.Fatalf("invalid response = %#v, %v", got, err)
	}
	files, err := filepath.Glob(filepath.Join(state, "diagnostics", "reviewer", "*-claude.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("diagnostic files = %v, %v; outcome = %#v", files, err, got)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic map[string]any
	if err := json.Unmarshal(data, &diagnostic); err != nil {
		t.Fatalf("decode diagnostic: %v", err)
	}
	if diagnostic["provider"] != "claude" || !strings.HasPrefix(diagnostic["stdout_head"].(string), stdout) {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestNativeReviewerDiagnosticRecordsFailedLaunchDetails(t *testing.T) {
	root, bin := nativeFixture(t)
	claude := filepath.Join(root, "claude"+filepath.Ext(bin))
	if err := os.Rename(bin, claude); err != nil {
		t.Fatal(err)
	}
	bin = claude
	state := filepath.Join(root, "state")
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("MOCK_EXIT_CODE", "2")
	args := nativeArgs(root, bin, NoMaterialFinding)
	args["reviewer_agent"] = "claude"
	args["reviewer_model"] = "claude-sonnet-4-5"
	args["reviewer_effort"] = "high"
	path := t.TempDir()
	t.Setenv("PATH", path)
	got, err := RunBridge(context.Background(), root, args, 30*time.Second)
	if err != nil || got.Attributes["error_code"] != "REVIEWER_PROCESS_FAILED" {
		t.Fatalf("failed reviewer = %#v, %v", got, err)
	}
	files, err := filepath.Glob(filepath.Join(state, "diagnostics", "reviewer", "*-claude.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("diagnostic files = %v, %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic map[string]any
	if err := json.Unmarshal(data, &diagnostic); err != nil {
		t.Fatalf("decode diagnostic: %v", err)
	}
	argv, ok := diagnostic["argv_flags"].([]any)
	if !ok {
		t.Fatalf("argv_flags = %#v", diagnostic["argv_flags"])
	}
	var promptOmitted, schemaOmitted bool
	for i, value := range argv {
		arg, ok := value.(string)
		if !ok {
			t.Fatalf("argv_flags[%d] = %#v", i, value)
		}
		if strings.Contains(arg, "You are Reviewer-1.") {
			t.Fatalf("prompt leaked in argv_flags: %q", arg)
		}
		if strings.HasPrefix(arg, "<omitted ") {
			promptOmitted = true
			if i > 0 && argv[i-1] == "--json-schema" {
				schemaOmitted = true
			}
		}
	}
	if !promptOmitted || !schemaOmitted {
		t.Fatalf("prompt/schema omission missing: %#v", argv)
	}
	if diagnostic["executable_path"] != bin || diagnostic["cwd"] == "" || !filepath.IsAbs(diagnostic["cwd"].(string)) || diagnostic["path_head"] != path {
		t.Fatalf("launch details = %#v", diagnostic)
	}
}

func TestNativeReviewerDiagnosticKeepsNewestTwenty(t *testing.T) {
	root, bin := nativeFixture(t)
	claude := filepath.Join(root, "claude"+filepath.Ext(bin))
	if err := os.Rename(bin, claude); err != nil {
		t.Fatal(err)
	}
	bin = claude
	state := filepath.Join(root, "state")
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("MOCK_OUTPUT", "invalid")
	args := nativeArgs(root, bin, NoMaterialFinding)
	args["reviewer_agent"] = "claude"
	args["reviewer_model"] = "claude-sonnet-4-5"
	args["reviewer_effort"] = "high"
	isolateReviewerPath(t)
	for i := 0; i < 21; i++ {
		if _, err := RunBridge(context.Background(), root, args, 30*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(state, "diagnostics", "reviewer", "*.json"))
	if err != nil || len(files) != 20 {
		t.Fatalf("diagnostic file count = %d, %v; want 20", len(files), err)
	}
}

type panicAdapter struct{ reviewerAdapter }

func (a panicAdapter) BuildCommand(string, map[string]any, map[string]any, CommandOptions) ([]string, error) {
	panic("test")
}
func registerPanicAdapter(t *testing.T) {
	t.Helper()
	if err := RegisterReviewerAdapter("panic-test", panicAdapter{reviewerAdapter{"agy"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reviewerAdapters["panic-test"] = nil; delete(reviewerAdapters, "panic-test") })
}
