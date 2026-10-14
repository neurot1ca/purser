package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"purser-hook/internal/jobs"
)

const remoteSchemas = `[{"name":"codex_start","description":"Start a durable Codex job.","inputSchema":{"type":"object","properties":{"prompt":{"type":"string"},"label":{"type":"string"},"model":{"type":"string"},"cwd":{"type":"string"},"config":{"type":"object"},"developer-instructions":{"type":"string"},"base-instructions":{"type":"string"},"compact-prompt":{"type":"string"},"sandbox":{"type":"string"},"approval-policy":{"type":"string"},"worktree":{"type":"boolean"},"timeoutSeconds":{"type":"number"}},"required":["prompt"]}},{"name":"codex_result","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"}},"required":["jobId"]}},{"name":"codex_cancel","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"reason":{"type":"string"}},"required":["jobId"]}},{"name":"codex_apply","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"},"commitMessage":{"type":"string","description":"Required for the plain apply step."},"acceptOnly":{"type":"boolean"},"revision":{"type":"string"},"reviewer1Valid":{"type":"integer","minimum":0},"reviewer1False":{"type":"integer","minimum":0},"resolved":{"type":"boolean"}},"required":["jobId"]}},{"name":"host_diff","inputSchema":{"type":"object","properties":{"jobId":{"type":"string"}},"required":["jobId"]}},{"name":"host_read","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]`

type remoteConfig struct{ repo, session string }

func remoteToolAllowed(name string) bool {
	switch name {
	case "codex_start", "codex_result", "codex_cancel", "codex_apply", "host_diff", "host_read":
		return true
	}
	return false
}
func sameRemotePath(a, b string) bool {
	aa, e1 := filepath.Abs(a)
	bb, e2 := filepath.Abs(b)
	if e1 != nil || e2 != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		aa = strings.ToLower(aa)
		bb = strings.ToLower(bb)
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}
func remoteDeny(v map[string]any) (bool, string) {
	h, _ := v["hookSpecificOutput"].(map[string]any)
	if h == nil {
		return false, ""
	}
	if h["permissionDecision"] == "deny" {
		s, _ := h["permissionDecisionReason"].(string)
		if s == "" {
			s = "denied by Purser enforcement"
		}
		return true, s
	}
	return false, ""
}
func remoteUpdatedInput(v map[string]any) map[string]any {
	h, _ := v["hookSpecificOutput"].(map[string]any)
	x, _ := h["updatedInput"].(map[string]any)
	return x
}
func remoteAdditionalContext(v map[string]any) string {
	h, _ := v["hookSpecificOutput"].(map[string]any)
	s, _ := h["additionalContext"].(string)
	return s
}
func remotePostText(v map[string]any) string {
	if text := remoteAdditionalContext(v); text != "" {
		return text
	}
	h, _ := v["hookSpecificOutput"].(map[string]any)
	for _, field := range []string{"updatedToolOutput", "updatedMCPToolOutput"} {
		items, _ := h[field].([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			if text, ok := m["text"].(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}
func remoteContent(v map[string]any) []any {
	if v == nil {
		return nil
	}
	c, _ := v["content"].([]any)
	return c
}
func remoteCommitApplied(repo string, args, result map[string]any) string {
	if result["isError"] == true {
		return ""
	}
	value, _ := result["structuredContent"].(map[string]any)
	applied := value != nil && value["applied"] == true
	var paths []string
	if applied {
		if list, ok := value["paths"].([]string); ok {
			for _, p := range list {
				rel := filepath.Clean(p)
				if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return "invalid applied path"
				}
				paths = append(paths, "./"+rel)
			}
		} else if list, ok := value["paths"].([]any); ok {
			for _, v := range list {
				if p, ok := v.(string); ok {
					rel := filepath.Clean(p)
					if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return "invalid applied path"
					}
					paths = append(paths, "./"+rel)
				}
			}
		}
	}
	if !applied {
		return ""
	}
	if messageErr := remoteCommitMessageError(args); messageErr != "" {
		return messageErr
	}
	msg := args["commitMessage"].(string)
	if len(paths) == 0 {
		return "applied job returned no paths"
	}
	run := func(name string, argv ...string) (string, error) {
		cmd := exec.Command(name, argv...)
		cmd.Dir = repo
		out, e := cmd.CombinedOutput()
		if e != nil {
			return strings.TrimSpace(string(out)), e
		}
		return strings.TrimSpace(string(out)), nil
	}
	if out, e := run("git", append([]string{"add", "--"}, paths...)...); e != nil {
		return "git add failed: " + out
	}
	if out, e := run("git", append(append([]string{"commit", "--only", "-m", msg, "--"}, paths...))...); e != nil {
		return "git commit failed: " + out
	}
	if out, e := run("git", "push", "origin", "HEAD"); e != nil {
		return "git push failed: " + out
	}
	hash, e := run("git", "rev-parse", "HEAD")
	if e != nil {
		return "git rev-parse failed: " + hash
	}
	result["commit"] = hash
	return ""
}

func remoteCommitMessageError(args map[string]any) string {
	msg, ok := args["commitMessage"].(string)
	if !ok || strings.TrimSpace(msg) == "" || len(msg) > 200 {
		return "commitMessage must be nonempty English text (max 200 characters)"
	}
	for _, r := range msg {
		if r > 127 {
			return "commitMessage must be English ASCII text"
		}
	}
	return ""
}

func remoteHostRead(c *remoteConfig, args map[string]any) map[string]any {
	p, ok := args["path"].(string)
	if !ok || strings.TrimSpace(p) == "" {
		return legacyError(nil, "path is outside the remote read allowlist")
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(p) {
		if clean == "NEXT.md" {
			p = filepath.Join(c.repo, "NEXT.md")
		} else {
			p = filepath.Join(stateRoot(), "plans", clean)
		}
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return legacyError(nil, err.Error())
	}
	if sameRemotePath(resolved, filepath.Join(c.repo, "NEXT.md")) {
		p = resolved
	} else {
		base, baseErr := filepath.EvalSymlinks(filepath.Join(stateRoot(), "plans"))
		rel, e := filepath.Rel(base, resolved)
		if baseErr != nil || e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return legacyError(nil, "path is outside the remote read allowlist")
		}
		p = resolved
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return legacyError(nil, err.Error())
	}
	if len(b) > 400<<10 {
		return legacyError(nil, "file exceeds 400 KB")
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
}
func remoteHostDiff(c *remoteConfig, args map[string]any) map[string]any {
	id, _ := args["jobId"].(string)
	if id == "" || filepath.Base(id) != id {
		return legacyError(nil, "invalid jobId")
	}
	rec, err := jobs.Read(filepath.Join(stateRoot(), "codex_exec_mcp", "jobs", id+".json"))
	if err != nil {
		return legacyError(nil, err.Error())
	}
	var worktree, snapshot string
	rec.Get("worktreePath", &worktree)
	rec.Get("snapshot", &snapshot)
	if worktree == "" || snapshot == "" {
		return legacyError(nil, "job has no worktree snapshot")
	}
	cmd := exec.Command("git", "add", "-N", ".")
	cmd.Dir = worktree
	if out, e := cmd.CombinedOutput(); e != nil {
		return legacyError(nil, strings.TrimSpace(string(out)))
	}
	cmd = exec.Command("git", "diff", snapshot)
	cmd.Dir = worktree
	out, e := cmd.Output()
	if e != nil {
		return legacyError(nil, e.Error())
	}
	if len(out) > 400<<10 {
		out = out[:400<<10]
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(out)}}}
}

func remoteMain(args []string) int {
	fs := flag.NewFlagSet("remote", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", "", "absolute repository path")
	addr := fs.String("addr", "127.0.0.1:8787", "loopback listen address")
	if fs.Parse(args) != nil {
		return 2
	}
	token := os.Getenv("PURSER_REMOTE_TOKEN")
	if len(token) < 32 {
		fmt.Fprintln(os.Stderr, "PURSER_REMOTE_TOKEN must contain at least 32 characters")
		return 2
	}
	if !strings.HasPrefix(*addr, "127.0.0.1:") {
		fmt.Fprintln(os.Stderr, "remote server must bind to 127.0.0.1")
		return 2
	}
	if !filepath.IsAbs(*repo) {
		fmt.Fprintln(os.Stderr, "--repo must be absolute")
		return 2
	}
	canonical, err := filepath.Abs(*repo)
	if err != nil {
		return 2
	}
	cfg := &remoteConfig{repo: canonical, session: "remote-process"}
	handler := remoteHTTPHandler(token, cfg)
	server := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func remoteHTTPHandler(token string, cfg *remoteConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tool, status := "", "ok"
		defer func() {
			fmt.Fprintf(os.Stderr, "%s tool=%s status=%s\n", time.Now().Format(time.RFC3339), tool, status)
		}()
		if r.Method == "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			status = "405"
			return
		}
		if r.Method != "POST" || r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			status = "404"
			return
		}
		header := r.Header.Get("Authorization")
		supplied := strings.TrimPrefix(header, "Bearer ")
		if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			status = "401"
			return
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if e != nil {
			http.Error(w, "bad request", 400)
			status = "400"
			return
		}
		var req map[string]json.RawMessage
		if json.Unmarshal(body, &req) != nil {
			http.Error(w, "bad json", 400)
			status = "400"
			return
		}
		if raw, ok := req["id"]; !ok || string(bytes.TrimSpace(raw)) == "null" {
			status = "202"
			w.WriteHeader(http.StatusAccepted)
			return
		}
		method := ""
		_ = json.Unmarshal(req["method"], &method)
		if method == "tools/list" {
			var id json.RawMessage
			id = req["id"]
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}`, id, remoteSchemas)
			return
		}
		if method == "tools/call" {
			var p map[string]json.RawMessage
			_ = json.Unmarshal(req["params"], &p)
			_ = json.Unmarshal(p["name"], &tool)
			if !remoteToolAllowed(tool) {
				status = "refused"
				mcp := rpcReply{"2.0", req["id"], nil, map[string]any{"code": -32602, "message": "tool is not available remotely"}}
				b, _ := json.Marshal(mcp)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(b)
				return
			}
		}
		var out bytes.Buffer
		srv := &mcpServer{ctx: context.Background(), out: &out, remote: cfg}
		srv.handle(body)
		srv.work.Wait()
		var reply map[string]any
		if json.Unmarshal(out.Bytes(), &reply) == nil {
			if result, ok := reply["result"].(map[string]any); ok && result["isError"] == true {
				status = "error"
			}
			if reply["error"] != nil {
				status = "error"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.TrimSpace(out.Bytes()))
	})
}
