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
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"purser-hook/internal/jobs"
	"purser-hook/internal/proc"
)

const mcpSchemas = `[{"name":"codex","description":"Run a new Codex thread.","inputSchema":{"type":"object","properties":{"prompt":{"type":"string"},"label":{"type":"string","description":"Short human task description in the user's language (up to 80 characters)."},"model":{"type":"string"},"cwd":{"type":"string"},"config":{"type":"object"},"developer-instructions":{"type":"string"},"base-instructions":{"type":"string"},"compact-prompt":{"type":"string"},"sandbox":{"type":"string","enum":["read-only","workspace-write","danger-full-access"]},"approval-policy":{"type":"string","enum":["on-request","never"]}},"required":["prompt"]}},{"name":"codex-reply","description":"Continue a thread created by this server.","inputSchema":{"type":"object","properties":{"threadId":{"type":"string"},"conversationId":{"type":"string","description":"Deprecated alias for threadId."},"prompt":{"type":"string"},"label":{"type":"string","description":"Short human task description in the user's language (up to 80 characters); may be omitted to reuse the thread label."}},"required":["prompt"]}},{"name":"codex_start","description":"Start a durable Codex job.","inputSchema":{"type":"object","properties":{"prompt":{"type":"string"},"label":{"type":"string","description":"Short human task description in the user's language (up to 80 characters)."},"model":{"type":"string"},"cwd":{"type":"string"},"config":{"type":"object"},"developer-instructions":{"type":"string"},"base-instructions":{"type":"string"},"compact-prompt":{"type":"string"},"sandbox":{"type":"string","enum":["read-only","workspace-write","danger-full-access"]},"approval-policy":{"type":"string","enum":["on-request","never"]},"call":{"type":"string"},"mode":{"type":"string","enum":["new","reply"]},"worktree":{"type":"boolean"},"threadId":{"type":"string"},"timeoutSeconds":{"type":"number"},"taskId":{"type":"string"},"route":{"type":"string"},"etaMinutes":{"type":"integer"},"jobModel":{"type":"string"},"jobEffort":{"type":"string"},"jobLabel":{"type":"string"},"jobPhase":{"type":"string"},"readinessCriteria":{"type":"object"},"readinessGrounded":{"type":"boolean"},"readinessAgent":{"type":"string"},"readinessModel":{"type":"string"}},"required":["prompt"]}},{"name":"codex_result","description":"Read a durable Codex job.","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"adopt":{"type":"boolean","description":"After user consent, finalize a job from an earlier session in this project."}},"required":["jobId"]}},{"name":"codex_cancel","description":"Cancel a durable Codex job.","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"reason":{"type":"string"}},"required":["jobId"]}},{"name":"codex_apply","description":"Apply a job worktree patch.","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"resolved":{"type":"boolean","description":"Record a hand-resolved conflict after verifying the repository."},"reviewer1Valid":{"type":"integer","minimum":0},"reviewer1False":{"type":"integer","minimum":0}},"required":["jobId"]}}]`

type mcpServer struct {
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
		ctx, cancel := context.WithCancel(context.Background())
		key := mcpIDKey(id)
		s.callsMu.Lock()
		if s.calls == nil {
			s.calls = make(map[string]*mcpCall)
		}
		s.calls[key] = &mcpCall{cancel: cancel}
		s.callsMu.Unlock()
		go s.runCall(ctx, key, id, params, cancel)
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

func mcpMain(args []string) int {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go startup(ctx, io.Discard, stderr)
	server := &mcpServer{out: output}
	reader := bufio.NewReader(input)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			server.handle(line)
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(stderr, err)
			}
			return 0
		}
	}
}

func startMCPMaintenance(ctx context.Context, stdout, stderr io.Writer) {
	if ctx.Err() != nil {
		return
	}
	if _, err := jobs.RecoverPending(stateRoot(), time.Now()); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "Codex job recovery unavailable: %v\n", err)
	}
	if ctx.Err() != nil {
		return
	}
	pluginRoot := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if pluginRoot == "" {
		executable, err := os.Executable()
		if err != nil {
			fmt.Fprintf(stderr, "Codex recovery scan unavailable: %v\n", err)
			return
		}
		pluginRoot = filepath.Dir(filepath.Dir(executable))
	}
	python := proc.ResolvePython(ctx, pluginRoot)
	if python == "" {
		if ctx.Err() == nil {
			fmt.Fprintln(stderr, "Codex job cleanup unavailable: Python was not found; recovery ran")
		}
		return
	}
	script := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator", "async_session_maintenance.py")
	cmd := mcpMaintenanceCommand(ctx, python, script, stateRoot(), stdout, stderr)
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "Codex recovery scan unavailable: %v\n", err)
	}
}

func mcpMaintenanceCommand(ctx context.Context, python, script, root string, stdout, stderr io.Writer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, python, script, "--startup", root)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	proc.ConfigureProcessTreeKill(cmd)
	return cmd
}
