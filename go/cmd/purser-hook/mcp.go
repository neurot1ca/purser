package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"purser-hook/internal/jobs"
	"purser-hook/internal/models"
	"purser-hook/internal/waiter"
)

const mcpSchemas = `[{"name":"codex","description":"Run a new Codex thread.","inputSchema":{"type":"object","properties":{"prompt":{"type":"string"},"label":{"type":"string","description":"Short human task description in the user's language (up to 80 characters)."},"model":{"type":"string"},"cwd":{"type":"string"},"config":{"type":"object"},"developer-instructions":{"type":"string"},"base-instructions":{"type":"string"},"compact-prompt":{"type":"string"},"sandbox":{"type":"string","enum":["read-only","workspace-write","danger-full-access"]},"approval-policy":{"type":"string","enum":["on-request","never"]}},"required":["prompt"]}},{"name":"codex-reply","description":"Continue a thread created by this server.","inputSchema":{"type":"object","properties":{"threadId":{"type":"string"},"conversationId":{"type":"string","description":"Deprecated alias for threadId."},"prompt":{"type":"string"},"label":{"type":"string","description":"Short human task description in the user's language (up to 80 characters); may be omitted to reuse the thread label."}},"required":["prompt"]}},{"name":"codex_start","description":"Start a durable Codex job.","inputSchema":{"type":"object","properties":{"prompt":{"type":"string"},"label":{"type":"string","description":"Short human task description in the user's language (up to 80 characters)."},"model":{"type":"string"},"cwd":{"type":"string"},"config":{"type":"object"},"developer-instructions":{"type":"string"},"base-instructions":{"type":"string"},"compact-prompt":{"type":"string"},"sandbox":{"type":"string","enum":["read-only","workspace-write","danger-full-access"]},"approval-policy":{"type":"string","enum":["on-request","never"]},"call":{"type":"string"},"mode":{"type":"string","enum":["new","reply"]},"worktree":{"type":"boolean"},"threadId":{"type":"string"},"timeoutSeconds":{"type":"number"},"taskId":{"type":"string"},"route":{"type":"string"},"etaMinutes":{"type":"integer"},"jobModel":{"type":"string"},"jobEffort":{"type":"string"},"jobLabel":{"type":"string"},"jobPhase":{"type":"string"},"readinessCriteria":{"type":"object"},"readinessGrounded":{"type":"boolean"},"readinessAgent":{"type":"string"},"readinessModel":{"type":"string"}},"required":["prompt"]}},{"name":"codex_result","description":"Read a durable Codex job.","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"adopt":{"type":"boolean","description":"After user consent, finalize a job from an earlier session in this project."}},"required":["jobId"]}},{"name":"codex_cancel","description":"Cancel a durable Codex job.","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"reason":{"type":"string"}},"required":["jobId"]}},{"name":"codex_apply","description":"Apply a job worktree patch.","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"resolved":{"type":"boolean","description":"Record a hand-resolved conflict after verifying the repository."},"reviewer1Valid":{"type":"integer","minimum":0},"reviewer1False":{"type":"integer","minimum":0},"acceptOnly":{"type":"boolean"},"sessionId":{"type":"string"},"revision":{"type":"string"}},"required":["jobId"]}}]`

type mcpServer struct {
	ctx     context.Context
	work    sync.WaitGroup
	out     io.Writer
	mu      sync.Mutex
	callsMu sync.Mutex
	calls   map[string]*mcpCall
}

type mcpCall struct {
	cancel context.CancelFunc
}

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

func (s *mcpServer) send(reply rpcReply) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(reply)
	if err != nil {
		return
	}
	data := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte(`\u2028`), []byte("\u2028"))
	data = bytes.ReplaceAll(data, []byte(`\u2029`), []byte("\u2029"))
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(data, '\n'))
}

func (s *mcpServer) handle(line []byte) {
	var message map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil || message == nil {
		return
	}
	var method string
	if json.Unmarshal(message["method"], &method) != nil {
		method = ""
	}
	var id json.RawMessage
	if raw := message["id"]; len(raw) > 0 && string(raw) != "null" {
		id = normalizeMCPString(raw)
	}
	if method == "notifications/cancelled" {
		var params map[string]json.RawMessage
		_ = json.Unmarshal(message["params"], &params)
		if raw := params["requestId"]; len(raw) != 0 && string(raw) != "null" {
			key := mcpIDKey(normalizeMCPString(raw))
			s.callsMu.Lock()
			if call := s.calls[key]; call != nil {
				call.cancel()
			}
			s.callsMu.Unlock()
		}
		return
	}
	if len(id) == 0 {
		return
	}

	result := any(nil)
	switch method {
	case "initialize":
		var params map[string]json.RawMessage
		_ = json.Unmarshal(message["params"], &params)
		var version any
		if raw := params["protocolVersion"]; len(raw) > 0 {
			version = normalizeMCPString(raw)
		}
		result = struct {
			ProtocolVersion any `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
			Capabilities struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
		}{version, struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}{"codex-exec-mcp", "1.0.0"}, struct {
			Tools map[string]any `json:"tools"`
		}{map[string]any{}}}
	case "ping":
		result = struct{}{}
	case "tools/list":
		result = struct {
			Tools json.RawMessage `json:"tools"`
		}{json.RawMessage(mcpSchemas)}
	case "tools/call":
		var params map[string]json.RawMessage
		if raw, present := message["params"]; present {
			if json.Unmarshal(raw, &params) != nil || params == nil {
				s.send(rpcReply{"2.0", id, nil, struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				}{-32602, "Invalid params"}})
				return
			}
		} else {
			params = map[string]json.RawMessage{}
		}
		parent := s.ctx
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithCancel(parent)
		key := mcpIDKey(id)
		s.callsMu.Lock()
		if s.calls == nil {
			s.calls = make(map[string]*mcpCall)
		}
		s.calls[key] = &mcpCall{cancel: cancel}
		s.callsMu.Unlock()
		s.work.Add(1)
		go func() { defer s.work.Done(); s.runCall(ctx, key, id, params, cancel) }()
		return
	default:
		s.send(rpcReply{"2.0", id, nil, struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{-32601, "Method not found"}})
		return
	}
	s.send(rpcReply{"2.0", id, result, nil})
}

func (s *mcpServer) runCall(ctx context.Context, key string, id json.RawMessage, params map[string]json.RawMessage, cancel context.CancelFunc) {
	var result map[string]any
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result = legacyError(nil, fmt.Sprint(recovered))
			}
		}()
		var name any
		if raw := params["name"]; len(raw) != 0 {
			_ = json.Unmarshal(raw, &name)
		}
		var args map[string]any
		if raw := params["arguments"]; len(raw) == 0 {
			args = map[string]any{}
		} else if json.Unmarshal(raw, &args) != nil || args == nil {
			result = legacyError(nil, "arguments must be an object")
			return
		}
		tool, ok := name.(string)
		if !ok {
			tool = pythonMCPValue(name)
		}
		switch tool {
		case "codex", "codex-reply", "codex_start", "codex_result", "codex_cancel", "codex_apply":
		default:
			result = legacyError(nil, "unknown tool: "+tool)
			return
		}
		value, err := dispatchOperation(ctx, tool, args, stateRoot())
		if err != nil {
			if tool == "codex_result" {
				jobID, ok := args["jobId"].(string)
				if !ok || jobID == "" {
					result = jobs.Result(stateRoot(), "", float64(time.Now().UnixNano())/1e9, jobs.ResultOptions{})
					return
				}
			}
			result = legacyError(nil, err.Error())
		} else if value == nil {
			result = legacyError(nil, "operation returned no result")
		} else {
			result = value
		}
	}()

	// Hold the call lock through send: cancellation either wins and suppresses
	// the response, or arrives after the complete frame has been written.
	s.callsMu.Lock()
	call := s.calls[key]
	if call != nil {
		delete(s.calls, key)
	}
	if call != nil && ctx.Err() == nil {
		s.send(rpcReply{"2.0", id, result, nil})
	}
	s.callsMu.Unlock()
	cancel()
}

func mcpIDKey(raw json.RawMessage) string {
	return string(bytes.TrimSpace(raw))
}

func pythonMCPValue(value any) string {
	switch value := value.(type) {
	case nil:
		return "None"
	case bool:
		if value {
			return "True"
		}
		return "False"
	case string:
		return value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

func marshalMCPString(value string) string {
	data, _ := json.Marshal(value)
	for _, escape := range []struct{ from, to string }{
		{`\u003c`, "<"}, {`\u003e`, ">"}, {`\u0026`, "&"},
		{`\u2028`, "\u2028"}, {`\u2029`, "\u2029"},
	} {
		data = bytes.ReplaceAll(data, []byte(escape.from), []byte(escape.to))
	}
	return string(data)
}

func normalizeMCPString(raw json.RawMessage) json.RawMessage {
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return json.RawMessage(marshalMCPString(value))
		}
	}
	return append(json.RawMessage(nil), raw...)
}

var discoverSessionOwner = waiter.SessionOwner

func mcpMain(args []string) int {
	owner, err := discoverSessionOwner()
	if err != nil {
		_ = os.Unsetenv("PURSER_SESSION_OWNER")
		fmt.Fprintf(os.Stderr, "Purser: session owner monitor failed: %v\n", err)
		return mcpMainWithStartup(args, os.Stdin, os.Stdout, os.Stderr, startMCPMaintenance)
	}
	if err := os.Setenv("PURSER_SESSION_OWNER", owner.String()); err != nil {
		_ = os.Unsetenv("PURSER_SESSION_OWNER")
		fmt.Fprintf(os.Stderr, "Purser: session owner monitor unavailable: %v\n", err)
		return mcpMainWithStartup(args, os.Stdin, os.Stdout, os.Stderr, startMCPMaintenance)
	}
	return mcpMainWithStartup(args, os.Stdin, os.Stdout, os.Stderr, startMCPMaintenance)
}

func mcpMainWithStartup(args []string, input io.Reader, output, stderr io.Writer,
	startup func(context.Context, io.Writer, io.Writer)) int {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	codexBin := flags.String("codex-bin", "", "Codex binary path or JSON argv prefix")
	if flags.Parse(args) != nil {
		return 2
	}
	if *codexBin != "" {
		_ = os.Setenv("PURSER_CODEX_BIN", *codexBin)
	}
	ctx, cancel, err := sessionContext(stderr)
	if err != nil {
		_ = os.Unsetenv("PURSER_SESSION_OWNER")
		fmt.Fprintf(stderr, "Purser: session owner monitor unavailable; serving without owner binding: %v\n", err)
		ctx, cancel = context.WithCancel(context.Background())
	}
	server := &mcpServer{out: output, ctx: ctx}
	server.work.Add(1)
	go func() { defer server.work.Done(); startup(ctx, io.Discard, stderr) }()
	defer func() {
		cancel()
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
		done := make(chan struct{})
		go func() { server.work.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			fmt.Fprintln(stderr, "Purser: session teardown unconfirmed after 4s")
		}
	}()
	type frame struct {
		line []byte
		err  error
	}
	frames := make(chan frame)
	go func() {
		reader := bufio.NewReader(input)
		for {
			line, err := reader.ReadBytes('\n')
			select {
			case frames <- frame{line, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return 0
		case frame := <-frames:
			if ctx.Err() != nil {
				return 0
			}
			if len(bytes.TrimSpace(frame.line)) > 0 {
				server.handle(frame.line)
			}
			if frame.err != nil {
				if frame.err != io.EOF {
					fmt.Fprintln(stderr, frame.err)
				}
				return 0
			}
		}
	}
}

func startMCPMaintenance(ctx context.Context, stdout, stderr io.Writer) {
	if ctx.Err() != nil {
		return
	}
	if _, err := jobs.RecoverCleanup(stateRoot(), time.Now()); err != nil {
		message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
		fmt.Fprintf(stderr, "Purser: cleanup recovery failed: %s\n", message)
	}
	enqueueCleanup(stateRoot())
	if ctx.Err() != nil {
		return
	}
	if err := models.RefreshCatalog(ctx, stateRoot(), time.Now()); err != nil {
		message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
		fmt.Fprintf(stderr, "Purser: model catalog refresh skipped: %s\n", message)
	}
}
