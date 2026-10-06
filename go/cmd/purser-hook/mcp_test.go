package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

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

func TestMCPToolSchemasMatchPythonOracle(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	python := "python"
	code := `import importlib.util,json,sys; sys.path.insert(0,sys.argv[1]); s=importlib.util.spec_from_file_location("oracle",sys.argv[2]); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); print(json.dumps(m.TOOL_SCHEMAS,ensure_ascii=False,separators=(",",":")))`
	cmd := exec.Command(python, "-c", code, filepath.Join(repo, ".claude", "hooks", "orchestrator"), filepath.Join(repo, ".claude", "hooks", "orchestrator", "codex_exec_mcp.py"))
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("load Python MCP oracle: %v", err)
	}
	if strings.TrimSpace(string(data)) != mcpSchemas {
		t.Fatal("embedded MCP schemas drifted from Python TOOL_SCHEMAS")
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
