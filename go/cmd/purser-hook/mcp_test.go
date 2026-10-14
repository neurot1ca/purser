package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"purser-hook/internal/jobs"
	"purser-hook/internal/waiter"
)

func TestMCPMonitorFailureServesProtocolUnbound(t *testing.T) {
	owner, err := waiter.PinOwner(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PURSER_SESSION_OWNER", owner.String())
	original := registerSessionOwner
	t.Cleanup(func() { registerSessionOwner = original })
	registerSessionOwner = func(context.Context, waiter.Owner, chan struct{}, io.Writer) error {
		return syscall.EACCES
	}
	output, stderr, input, serverDone := runMCPWithStartup(func(context.Context, io.Writer, io.Writer) {})
	if _, err := io.WriteString(input, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(input, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for strings.Count(output.String(), "\n") < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := output.String(); !strings.Contains(got, `"id":1`) || !strings.Contains(got, `"id":2`) {
		t.Fatalf("protocol unavailable after monitor failure: %s", got)
	}
	if strings.Contains(output.String(), "session owner monitor") {
		t.Fatalf("monitor warning contaminated protocol stdout: %s", output.String())
	}
	if strings.Count(stderr.String(), "serving without owner binding") != 1 {
		t.Fatalf("want one fail-open warning, got %q", stderr.String())
	}
	if got := os.Getenv("PURSER_SESSION_OWNER"); got != "" {
		t.Fatalf("owner token remained bound after monitor failure: %q", got)
	}
	_ = input.Close()
	if code := <-serverDone; code != 0 {
		t.Fatalf("MCP exit code = %d", code)
	}
}

func TestMCPMaintenanceChild(t *testing.T) {
	if os.Getenv("PURSER_MCP_CHILD") != "1" {
		return
	}
	time.Sleep(time.Duration(envInt("PURSER_MCP_CHILD_DELAY_MS")) * time.Millisecond)
	_, _ = fmt.Fprint(os.Stdout, "maintenance stdout sentinel")
	_, _ = fmt.Fprint(os.Stderr, "maintenance stderr sentinel")
	os.Exit(envInt("PURSER_MCP_CHILD_EXIT"))
}

func envInt(key string) int {
	value, _ := strconv.Atoi(os.Getenv(key))
	return value
}

func TestMCPStartupRecoversAndEnqueuesCleanupWithoutInlineSweep(t *testing.T) {
	scratch, err := filepath.Abs("../../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(scratch, "mcp-maintenance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	pluginRoot := filepath.Join(base, "plugin")
	stateRoot := filepath.Join(base, "state")
	pathDir := filepath.Join(base, "empty-path")
	if err := os.MkdirAll(pathDir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("PURSER_STATE_DIR", stateRoot)
	t.Setenv("PATH", pathDir)
	catalogDir := filepath.Join(stateRoot, "model-catalog")
	if err := os.MkdirAll(catalogDir, 0700); err != nil {
		t.Fatal(err)
	}
	catalog := fmt.Sprintf(`{"fetched_at":%q,"dropped_count":0,"providers":{"provider":{"models":{"model-a":{"id":"model-a","name":"Model A","reasoning":true,"limits":{"context":1000,"output":100}}}}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(catalogDir, "catalog.json"), []byte(catalog), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	jobsDir := filepath.Join(stateRoot, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		t.Fatal(err)
	}
	job := []byte(`{"jobId":"` + id + `","state":"QUEUED"}`)
	if err := os.WriteFile(filepath.Join(jobsDir, id+".json"), job, 0600); err != nil {
		t.Fatal(err)
	}
	staleTemp := filepath.Join(stateRoot, "cache", "go-tmp", "stale")
	if err := os.MkdirAll(staleTemp, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staleTemp, old, old); err != nil {
		t.Fatal(err)
	}
	oldRequest := requestCleanup
	t.Cleanup(func() { requestCleanup = oldRequest })
	starts := 0
	requestCleanup = func(root string) error {
		return jobs.RequestCleanup(root, func() (int, int64, error) {
			starts++
			return 0, 0, nil
		})
	}

	var stdout, stderr bytes.Buffer
	startMCPMaintenance(context.Background(), &stdout, &stderr)
	if _, err := os.Stat(filepath.Join(jobsDir, id+".owner-lock")); err != nil {
		t.Fatalf("startup did not run native recovery: %v", err)
	}
	if _, err := os.Stat(staleTemp); err != nil {
		t.Errorf("startup ran the expensive temp sweep: %v", err)
	}
	if dirty, err := os.ReadFile(filepath.Join(stateRoot, "cleanup", "dirty")); err != nil || len(dirty) == 0 || starts != 1 {
		t.Errorf("cleanup request not recorded for child: dirty=%q starts=%d err=%v", dirty, starts, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("maintenance contaminated protocol stdout: %q", stdout.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("native maintenance wrote unexpected stderr: %q", got)
	}
}

func mcpTestStartup(delay time.Duration, exitCode int, done chan<- struct{}) func(context.Context, io.Writer, io.Writer) {
	return func(ctx context.Context, stdout, stderr io.Writer) {
		defer close(done)
		cmd := testutil.Hide(exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPMaintenanceChild$"))
		cmd.Env = append(os.Environ(), "PURSER_MCP_CHILD=1",
			"PURSER_MCP_CHILD_DELAY_MS="+strconv.Itoa(int(delay/time.Millisecond)),
			"PURSER_MCP_CHILD_EXIT="+strconv.Itoa(exitCode))
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "Codex recovery scan unavailable: %v\n", err)
		}
	}
}

func runMCPWithStartup(startup func(context.Context, io.Writer, io.Writer)) (*testBuffer, *testBuffer, *io.PipeWriter, <-chan int) {
	input, inputWriter := io.Pipe()
	output, stderr := &testBuffer{}, &testBuffer{}
	serverDone := make(chan int, 1)
	go func() { serverDone <- mcpMainWithStartup(nil, input, output, stderr, startup) }()
	return output, stderr, inputWriter, serverDone
}

func TestMCPStartupMaintenanceDoesNotDelayInitialize(t *testing.T) {
	startupDone := make(chan struct{})
	output, _, input, serverDone := runMCPWithStartup(
		mcpTestStartup(700*time.Millisecond, 0, startupDone))
	defer input.Close()
	started := time.Now()
	_, _ = io.WriteString(input, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n")
	deadline := time.Now().Add(300 * time.Millisecond)
	for output.String() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if output.String() == "" || time.Since(started) >= 300*time.Millisecond {
		t.Fatalf("initialize waited for startup maintenance: %q", output.String())
	}
	if !strings.Contains(output.String(), `"id":1`) {
		t.Fatalf("initialize response missing: %s", output.String())
	}
	_ = input.Close()
	<-serverDone
}

func TestMCPStartupMaintenanceOutputNeverReachesProtocolStdout(t *testing.T) {
	startupDone := make(chan struct{})
	output, stderr, input, serverDone := runMCPWithStartup(
		mcpTestStartup(0, 0, startupDone))
	_, _ = io.WriteString(input, `{"jsonrpc":"2.0","id":2,"method":"initialize"}`+"\n")
	<-startupDone
	_ = input.Close()
	<-serverDone
	if strings.Contains(output.String(), "maintenance ") {
		t.Fatalf("maintenance output contaminated MCP stdout: %q", output.String())
	}
	if !strings.Contains(stderr.String(), "maintenance stderr sentinel") {
		t.Fatalf("maintenance stderr was not surfaced: %q", stderr.String())
	}
}

func TestMCPStartupMaintenanceFailureOnlyUsesStderr(t *testing.T) {
	startupDone := make(chan struct{})
	output, stderr, input, serverDone := runMCPWithStartup(
		mcpTestStartup(0, 7, startupDone))
	_, _ = io.WriteString(input, `{"jsonrpc":"2.0","id":3,"method":"initialize"}`+"\n")
	<-startupDone
	_ = input.Close()
	<-serverDone
	if strings.Contains(output.String(), "maintenance") {
		t.Fatalf("failed maintenance wrote to MCP stdout: %q", output.String())
	}
	if !strings.Contains(stderr.String(), "maintenance stderr sentinel") ||
		!strings.Contains(stderr.String(), "Codex recovery scan unavailable") {
		t.Fatalf("failure was not surfaced on stderr: %q", stderr.String())
	}
}

func exchange(line string) string {
	out := &testBuffer{}
	(&mcpServer{out: out}).handle([]byte(line))
	if strings.Contains(line, `"method":"tools/call"`) {
		deadline := time.Now().Add(3 * time.Second)
		for out.String() == "" && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	return out.String()
}

type testBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *testBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *testBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestMCPDiscoveryProtocol(t *testing.T) {
	got := exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18 🧵"}}`)
	want := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18 🧵","serverInfo":{"name":"codex-exec-mcp","version":"1.0.0"},"capabilities":{"tools":{}}}}` + "\n"
	if got != want {
		t.Fatalf("initialize mismatch:\n%s", got)
	}
	if got = exchange(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); got != `{"jsonrpc":"2.0","id":2,"result":{}}`+"\n" {
		t.Fatalf("ping: %s", got)
	}

	var gotList struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	got = exchange(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if err := json.Unmarshal([]byte(got), &gotList); err != nil {
		t.Fatal(err)
	}
	if string(gotList.Result.Tools) != mcpSchemas {
		t.Fatal("tools/list schema differs from embedded oracle")
	}
}

func TestMCPProtocolErrorsMalformedAndNotifications(t *testing.T) {
	if got := exchange(`{"jsonrpc":"2.0","id":"x","method":"no/such"}`); got != `{"jsonrpc":"2.0","id":"x","error":{"code":-32601,"message":"Method not found"}}`+"\n" {
		t.Fatalf("unknown method: %s", got)
	}
	if got := exchange(`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":[]}`); got != `{"jsonrpc":"2.0","id":22,"error":{"code":-32602,"message":"Invalid params"}}`+"\n" {
		t.Fatalf("invalid params: %s", got)
	}
	var out bytes.Buffer
	s := &mcpServer{out: &out}
	s.handle([]byte("{not json}"))
	s.handle([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	s.handle([]byte(`{"jsonrpc":"2.0","id":null,"method":"ping"}`))
	s.handle([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":12}}`))
	s.handle([]byte(`{"jsonrpc":"2.0","id":23,"method":"ping"}`))
	if want := `{"jsonrpc":"2.0","id":23,"result":{}}` + "\n"; out.String() != want {
		t.Fatalf("malformed/notification handling: %q", out.String())
	}
}

func TestMCPPreservesLargeJSONRPCIDAndUnknownToolResult(t *testing.T) {
	id := "900719925474099312345"
	got := exchange(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
	if !strings.Contains(got, `"id":`+id) {
		t.Fatalf("lossy id: %s", got)
	}
	got = exchange(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"not-a-tool","arguments":{}}}`)
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result.IsError || response.Result.Content[0].Text != `{"threadId": null, "error": "unknown tool: not-a-tool"}` {
		t.Fatalf("unknown tool result: %s", got)
	}
}

func TestMCPCallValidatesArgumentsBeforeToolNameAndPreservesResultShape(t *testing.T) {
	got := exchange(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"not-a-tool","arguments":[]}}`)
	if !strings.Contains(got, `"text":"{\"threadId\": null, \"error\": \"arguments must be an object\"}"`) ||
		!strings.Contains(got, `"isError":true`) {
		t.Fatalf("invalid arguments should be a tool error before unknown-name handling: %s", got)
	}
	got = exchange(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"codex","arguments":{"prompt":3}}}`)
	if !strings.Contains(got, `"text":"{\"threadId\": null, \"error\": \"prompt must be a string\"}"`) ||
		!strings.Contains(got, `"isError":true`) {
		t.Fatalf("dispatcher error shape: %s", got)
	}
}

func TestMCPSendUsesPythonCompatibleUnicodeAndHTMLSerialization(t *testing.T) {
	got := exchange(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"<>&   "}}`)
	want := "{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"protocolVersion\":\"<>&   \",\"serverInfo\":{\"name\":\"codex-exec-mcp\",\"version\":\"1.0.0\"},\"capabilities\":{\"tools\":{}}}}\n"
	if got != want {
		t.Fatalf("serialization differs from Python ensure_ascii=False: %q", got)
	}
}

func TestMCPSingleWriterSerializesFrames(t *testing.T) {
	var out bytes.Buffer
	s := &mcpServer{out: &out}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "ping"})
			s.handle(raw)
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 32 {
		t.Fatalf("got %d complete frames", len(lines))
	}
	for _, line := range lines {
		var reply map[string]any
		if json.Unmarshal([]byte(line), &reply) != nil {
			t.Fatalf("interleaved frame: %q", line)
		}
	}
}
