package enforce

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/waiter"
)

const MCPHandoffLimit = 2 << 20

const (
	mcpHandoffSweepAge = time.Hour
	mcpHandoffSweepMax = 256
)

// MCPHandoff is private host metadata, never decoded from tool arguments. An
// empty AgentID is verified main Claude; unknown callers must use "unknown".
// Env session identity is deliberately absent: it can be stale after continue.
type MCPHandoff struct {
	Owner     waiter.Owner   `json:"owner"`
	SessionID string         `json:"session_id"`
	ToolUseID string         `json:"tool_use_id"`
	Tool      string         `json:"tool_name"`
	Cwd       string         `json:"cwd"`
	AgentID   string         `json:"agent_id"`
	Created   time.Time      `json:"created"`
	Input     map[string]any `json:"input"`
	Effective map[string]any `json:"effective,omitempty"`
	Response  map[string]any `json:"response,omitempty"`
	Prepared  bool           `json:"prepared"`
}

func (h *MCPHandoff) event() map[string]any {
	e := map[string]any{"hook_event_name": "PreToolUse", "session_id": h.SessionID,
		"tool_use_id": h.ToolUseID, "tool_name": h.Tool, "cwd": h.Cwd, "tool_input": h.Input}
	if h.AgentID != "" {
		e["agent_id"] = h.AgentID
	}
	return e
}

// handoffPath checks every component before opening anything. Go Lstat exposes
// Windows junctions as ModeSymlink and other reparse points as ModeIrregular.
// This protects the host boundary, not against malicious concurrent same-user
// filesystem mutation. Windows inherits the private state directory's ACL.
func handoffPath(root string, owner waiter.Owner, id string) (string, error) {
	if id == "" || len(id) > 512 {
		return "", errors.New("missing/oversized MCP tool-use identity")
	}
	actual, err := waiter.PinOwner(owner.PID)
	if err != nil || actual != owner {
		return "", errors.New("stale MCP process owner")
	}
	dir, err := filepath.Abs(filepath.Join(root, "mcp-handoffs"))
	if err != nil {
		return "", err
	}
	var ensure func(string) error
	ensure = func(p string) error {
		if parent := filepath.Dir(p); parent != p {
			if err := ensure(parent); err != nil {
				return err
			}
		}
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			if err = os.Mkdir(p, 0700); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(p)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeType != os.ModeDir {
			return fmt.Errorf("unsafe handoff directory %s", p)
		}
		return nil
	}
	if err := ensure(dir); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0700); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(owner.String() + "\x00" + id))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

func handoffFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("unsafe handoff file")
	}
	return nil
}

func handoffLock(path string) (func() error, error) {
	if err := handoffFile(path + ".lock"); err != nil {
		return nil, err
	}
	return contextio.LockFile(path + ".lock")
}

func writeMCPHandoff(path string, h *MCPHandoff) error {
	if err := handoffFile(path); err != nil {
		return err
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(data) > MCPHandoffLimit {
		return errors.New("MCP handoff too large")
	}
	// Reuse the enforcement atomic writer (unique 0600 temp, sync, close,
	// rename), with bounded transient retries like jobs' writer. jobs
	// imports enforce, so its writer cannot be imported here.
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = atomicWrite(path, data)
		if err == nil {
			break
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) || !(errno == syscall.EACCES || errno == syscall.EBUSY ||
			(runtime.GOOS == "windows" && (errno == 5 || errno == 32 || errno == 33))) || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return syncHandoffDir(path)
}

func syncHandoffDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func isMCPHandoffBase(name string) bool {
	if len(name) != 69 || !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := hex.DecodeString(name[:64])
	return err == nil
}

// sweepMCPHandoffs removes only old, regular handoff artifacts. It runs while
// publish.lock is held; failures (including Windows sharing/AV locks) are
// intentionally ignored so a blocked cleanup can be retried by a later call.
// A record is removed before its claim tombstone, so a failed record removal
// can never turn a still-live claim into a replayable call.
func sweepMCPHandoffs(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-mcpHandoffSweepAge)
	type artifact struct {
		path string
	}
	type group struct {
		record, claim, lock artifact
		temps               []artifact
	}
	groups := make(map[string]*group)
	for _, entry := range entries {
		name := entry.Name()
		base := ""
		kind := ""
		switch {
		case isMCPHandoffBase(name):
			base, kind = name, "record"
		case strings.HasSuffix(name, ".claim") && len(name) > len(".claim"):
			base, kind = strings.TrimSuffix(name, ".claim"), "claim"
		case strings.HasSuffix(name, ".lock") && len(name) > len(".lock"):
			base, kind = strings.TrimSuffix(name, ".lock"), "lock"
		default:
			// atomicWrite names its temporary file <record>.<random>.tmp.
			if strings.HasSuffix(name, ".tmp") {
				if dot := strings.IndexByte(name, '.'); dot > 0 {
					base, kind = name[:dot]+".json", "temp"
				}
			}
		}
		if kind == "" || !isMCPHandoffBase(base) {
			continue
		}
		info, statErr := os.Lstat(filepath.Join(dir, name))
		if statErr != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		g := groups[base]
		if g == nil {
			g = &group{}
			groups[base] = g
		}
		a := artifact{path: filepath.Join(dir, name)}
		switch kind {
		case "record":
			g.record = a
		case "claim":
			g.claim = a
		case "lock":
			g.lock = a
		case "temp":
			g.temps = append(g.temps, a)
		}
	}
	removed := 0
	for _, g := range groups {
		if removed >= mcpHandoffSweepMax {
			return
		}
		if g.record.path != "" {
			if err := os.Remove(g.record.path); err != nil {
				continue
			}
			removed++
		}
		for _, item := range append([]artifact{g.claim, g.lock}, g.temps...) {
			if removed >= mcpHandoffSweepMax {
				return
			}
			if item.path == "" {
				continue
			}
			if err := os.Remove(item.path); err == nil {
				removed++
			}
		}
	}
}

// PublishMCPHandoff does no routing or baseline acquisition. Duplicate publication
// is rejected even after a crash; tombstones are retained for the owner's life.
func PublishMCPHandoff(root string, owner waiter.Owner, event map[string]any) error {
	h := &MCPHandoff{Owner: owner, SessionID: stringValue(event["session_id"]), ToolUseID: stringValue(event["tool_use_id"]),
		Tool: stringValue(event["tool_name"]), Cwd: stringValue(event["cwd"]), Created: time.Now().UTC()}
	var ok bool
	h.Input, ok = event["tool_input"].(map[string]any)
	if !ok || h.Input == nil || h.SessionID == "" || len(h.SessionID) > 512 || !filepath.IsAbs(h.Cwd) || !codexTools[h.Tool] {
		return errors.New("invalid MCP handoff identity/input")
	}
	// Presence and type matter: absent metadata is unknown, never main authority.
	h.AgentID = "unknown"
	if agent, known := event["agent_id"].(string); known {
		h.AgentID = agent
	}
	path, err := handoffPath(root, owner, h.ToolUseID)
	if err != nil {
		return err
	}
	// Bound total records and serialize publication, including capacity checks.
	unlock, err := handoffLock(filepath.Join(filepath.Dir(path), "publish"))
	if err != nil {
		return err
	}
	defer unlock()
	sweepMCPHandoffs(filepath.Dir(path), time.Now())
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	if len(entries) >= 4096 {
		return errors.New("MCP handoff capacity exhausted")
	}
	for _, p := range []string{path, path + ".claim"} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return errors.New("duplicate MCP handoff")
		}
	}
	return writeMCPHandoff(path, h)
}

func readMCPHandoff(path string) (*MCPHandoff, error) {
	if err := handoffFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MCPHandoffLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MCPHandoffLimit {
		return nil, errors.New("MCP handoff too large")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	// Reject duplicate and missing identity fields before struct decoding can
	// collapse them. Records are written only by this package.
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("invalid MCP handoff record")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("invalid handoff field")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate handoff field")
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	for _, key := range []string{"owner", "session_id", "tool_use_id", "tool_name", "cwd", "agent_id", "created", "input"} {
		if raw := fields[key]; len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return nil, errors.New("missing handoff identity")
		}
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var h MCPHandoff
	if err = d.Decode(&h); err != nil {
		return nil, err
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing handoff data")
	}
	return &h, nil
}

// ClaimMCPHandoff consumes only an exact binding. Arguments use encoding/json
// sorted object keys with UseNumber on wire decoding (no policy hash changes).
// The separate exclusive, synced tombstone survives failed replacement, crashes,
// cancellation and cleanup failures. Never remove it to retry a call.
func ClaimMCPHandoff(root string, owner waiter.Owner, id, tool string, input map[string]any) (*MCPHandoff, error) {
	path, err := handoffPath(root, owner, id)
	if err != nil {
		return nil, err
	}
	// Missing/untrusted call IDs cannot grow the directory with lock files.
	if _, err := os.Lstat(path); err != nil {
		return nil, err
	}
	unlock, err := handoffLock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	h, err := readMCPHandoff(path)
	if err != nil {
		return nil, err
	}
	a, e1 := json.Marshal(input)
	b, e2 := json.Marshal(h.Input)
	if h.Owner != owner || h.ToolUseID != id || h.SessionID == "" || h.Tool != tool || !codexTools[tool] ||
		h.Created.IsZero() || time.Since(h.Created) < 0 || time.Since(h.Created) > 10*time.Minute ||
		e1 != nil || e2 != nil || input == nil || !bytes.Equal(a, b) {
		return nil, errors.New("MCP handoff binding/lifecycle mismatch")
	}
	f, err := os.OpenFile(path+".claim", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("MCP handoff already claimed/unavailable: %w", err)
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := syncHandoffDir(path); err != nil {
		return nil, err
	}
	return h, nil
}

// PrepareMCP runs policy exactly once after a trusted claim, without dispatch.
// Binding failures always deny; only internal preparation/persistence failures
// use the existing synchronous fail-open contract.
func PrepareMCP(root string, owner waiter.Owner, id, tool string, input map[string]any) (*MCPHandoff, error) {
	h, err := ClaimMCPHandoff(root, owner, id, tool, input)
	if err != nil {
		return nil, err
	}
	event := h.event()
	h.Response, err = Pre(event, root)
	if err != nil && strings.HasSuffix(tool, "__codex_apply") && (input["acceptOnly"] == true || h.AgentID != "") {
		h.Response = denyPre("MCP_PREPARATION_FAILED", "acceptance/apply preparation failed")
	}
	h.Effective = h.Input
	output, _ := h.Response["hookSpecificOutput"].(map[string]any)
	if updated, ok := output["updatedInput"].(map[string]any); ok {
		h.Effective = updated
	}
	h.Prepared = true
	path, pathErr := handoffPath(root, owner, id)
	if pathErr == nil {
		pathErr = writeMCPHandoff(path, h)
	}
	if pathErr != nil {
		if output["permissionDecision"] == "deny" {
			return h, pathErr
		}
		if strings.HasSuffix(tool, "__codex_apply") {
			if input["acceptOnly"] == true || h.AgentID != "" {
				h.Response = denyPre("MCP_PREPARATION_FAILED", "acceptance/apply preparation was not durable")
				return h, pathErr
			}
		}
		h.Response = preFailure(event, root, pathErr, map[string]any{})
		h.Effective = h.Input
		return h, pathErr
	}
	return h, err
}

// ReadPreparedMCPHandoff supplies effective arguments/context for later post
// intake. It cannot authorize dispatch and never makes a claim reusable.
func ReadPreparedMCPHandoff(root string, owner waiter.Owner, session, id string) (*MCPHandoff, error) {
	path, err := handoffPath(root, owner, id)
	if err != nil {
		return nil, err
	}
	if err := handoffFile(path + ".claim"); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path + ".claim"); err != nil {
		return nil, err
	}
	h, err := readMCPHandoff(path)
	if err != nil {
		return nil, err
	}
	if h.Owner != owner || h.SessionID != session || session == "" || h.ToolUseID != id || !h.Prepared {
		return nil, errors.New("MCP prepared identity mismatch")
	}
	return h, nil
}
