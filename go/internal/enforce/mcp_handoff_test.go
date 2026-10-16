package enforce

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"purser-hook/internal/testutil"
	"purser-hook/internal/waiter"
)

func mcpFixture(t *testing.T, id, tool string) (string, waiter.Owner, map[string]any) {
	t.Helper()
	root := preTempDir(t)
	owner, err := waiter.PinOwner(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	e := map[string]any{"session_id": "hook-session", "tool_use_id": id, "tool_name": "mcp__plugin_purser_codex-main__" + tool,
		"cwd": root, "agent_id": "", "tool_input": map[string]any{"prompt": "same", "sessionId": "untrusted"}}
	return root, owner, e
}

func TestMCPHandoffBindingReplay(t *testing.T) {
	root, owner, e := mcpFixture(t, "call", "codex")
	if err := PublishMCPHandoff(root, owner, e); err != nil {
		t.Fatal(err)
	}
	tool := stringValue(e["tool_name"])
	input := e["tool_input"].(map[string]any)
	if err := PublishMCPHandoff(root, owner, e); err == nil {
		t.Fatal("duplicate publication")
	}
	for _, item := range []struct {
		id, tool string
		input    map[string]any
	}{
		{"", tool, input}, {"missing", tool, input}, {"call", tool + "_start", input}, {"call", tool, map[string]any{"prompt": "changed"}},
	} {
		if _, err := ClaimMCPHandoff(root, owner, item.id, item.tool, item.input); err == nil {
			t.Fatal("binding accepted", item)
		}
	}
	h, err := ClaimMCPHandoff(root, owner, "call", tool, input)
	if err != nil {
		t.Fatal(err)
	}
	if h.event()["session_id"] != "hook-session" {
		t.Fatal("untrusted identity won")
	}
	if agent, ok := h.event()["agent_id"]; !ok || agent != "" {
		t.Fatalf("main identity was not preserved: %#v", h.event())
	}
	if _, err := ClaimMCPHandoff(root, owner, "call", tool, input); err == nil {
		t.Fatal("replayed claim")
	}
	path, _ := handoffPath(root, owner, "call")
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("public record")
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(root, "redirect")
		if err := os.Symlink(filepath.Dir(path), link); err != nil {
			t.Fatal(err)
		}
		if _, err := handoffPath(link, owner, "other"); err == nil {
			t.Fatal("followed symlink")
		}
	}
}

func TestMCPHandoffConcurrentClaims(t *testing.T) {
	if path := os.Getenv("PURSER_MCP_CLAIM_TEST"); path != "" {
		var h MCPHandoff
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &h) != nil {
			os.Exit(2)
		}
		_, err = ClaimMCPHandoff(filepath.Dir(filepath.Dir(path)), h.Owner, h.ToolUseID, h.Tool, h.Input)
		if err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	root, owner, e := mcpFixture(t, "race", "codex")
	if err := PublishMCPHandoff(root, owner, e); err != nil {
		t.Fatal(err)
	}
	path, _ := handoffPath(root, owner, "race")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestMCPHandoffConcurrentClaims$"))
			cmd.Env = append(os.Environ(), "PURSER_MCP_CLAIM_TEST="+path)
			results <- cmd.Run()
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Fatal(err)
			}
		}
	}
	if wins != 1 {
		t.Fatalf("claim winners: %d", wins)
	}
	// Equal arguments under separate call IDs remain independent.
	for _, id := range []string{"parallel-a", "parallel-b"} {
		e["tool_use_id"] = id
		if err := PublishMCPHandoff(root, owner, e); err != nil {
			t.Fatal(err)
		}
		if _, err := ClaimMCPHandoff(root, owner, id, stringValue(e["tool_name"]), e["tool_input"].(map[string]any)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMCPHandoffStaleLifecycle(t *testing.T) {
	root, owner, e := mcpFixture(t, "old", "codex")
	if err := PublishMCPHandoff(root, owner, e); err != nil {
		t.Fatal(err)
	}
	stale := owner
	stale.Created++
	if _, err := ClaimMCPHandoff(root, stale, "old", stringValue(e["tool_name"]), e["tool_input"].(map[string]any)); err == nil {
		t.Fatal("PID reuse accepted")
	}
	path, _ := handoffPath(root, owner, "old")
	h, _ := readMCPHandoff(path)
	h.Created = time.Now().Add(-11 * time.Minute)
	if err := writeMCPHandoff(path, h); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimMCPHandoff(root, owner, "old", h.Tool, h.Input); err == nil {
		t.Fatal("expired call accepted")
	}
}

func TestMCPHandoffWriteLockRecovery(t *testing.T) {
	root, owner, e := mcpFixture(t, "recover", "codex")
	path, _ := handoffPath(root, owner, "recover")
	lockPath := filepath.Join(filepath.Dir(path), "publish.lock")
	if err := os.Mkdir(lockPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := PublishMCPHandoff(root, owner, e); err == nil {
		t.Fatal("lock failure ignored")
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := PublishMCPHandoff(root, owner, e); err != nil {
		t.Fatal(err)
	}
	h, err := ClaimMCPHandoff(root, owner, "recover", stringValue(e["tool_name"]), e["tool_input"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	// A failed replacement retains the claim even if the original file is lost.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPHandoff(path, h); err == nil {
		t.Fatal("write failure ignored")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPHandoff(path, h); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// os.Open omits FILE_SHARE_DELETE: replacement must retry until the
		// reader releases its handle, without deleting the destination.
		held, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { time.Sleep(100 * time.Millisecond); _ = held.Close(); close(done) }()
		err = writeMCPHandoff(path, h)
		<-done
		if err != nil {
			t.Fatalf("sharing violation recovery: %v", err)
		}
	}
	if _, err := ClaimMCPHandoff(root, owner, "recover", h.Tool, h.Input); err == nil {
		t.Fatal("failed write reopened claim")
	}
}

func TestMCPHandoffSweepCapacityAndFreshClaim(t *testing.T) {
	root, owner, stale := mcpFixture(t, "stale-sweep", "codex")
	if err := PublishMCPHandoff(root, owner, stale); err != nil {
		t.Fatal(err)
	}
	stalePath, err := handoffPath(root, owner, "stale-sweep")
	if err != nil {
		t.Fatal(err)
	}
	staleRecord, err := readMCPHandoff(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	// Leave every artifact old enough for the sweep, including an interrupted
	// atomic-write temporary file. The record is removed before its claim.
	staleClaim := stalePath + ".claim"
	staleLock := stalePath + ".lock"
	staleTemp := stalePath + ".stale.tmp"

	fresh := stale
	fresh["tool_use_id"] = "fresh-claim"
	if err := PublishMCPHandoff(root, owner, fresh); err != nil {
		t.Fatal(err)
	}
	freshInput := fresh["tool_input"].(map[string]any)
	freshTool := stringValue(fresh["tool_name"])
	if _, err := ClaimMCPHandoff(root, owner, "fresh-claim", freshTool, freshInput); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPHandoff(stalePath, staleRecord); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{staleClaim, staleLock, staleTemp} {
		if err := os.WriteFile(path, []byte("stale"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{stalePath, staleClaim, staleLock, staleTemp} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	dir := filepath.Dir(stalePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Fill the private directory to the existing 4096-entry cap. These are
	// unrelated private files; sweep must only remove stale handoff artifacts.
	for i := len(entries); i < 4096; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(".capacity-%04d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	newCall := stale
	newCall["tool_use_id"] = "after-sweep"
	if err := PublishMCPHandoff(root, owner, newCall); err != nil {
		t.Fatalf("publish at capacity after sweep: %v", err)
	}
	for _, path := range []string{stalePath, staleClaim, staleLock, staleTemp} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("stale artifact remains %s: %v", path, err)
		}
	}
	if err := PublishMCPHandoff(root, owner, fresh); err == nil {
		t.Fatal("fresh claim tombstone did not block duplicate publish")
	}
}

func TestMCPPreparationFailureSemantics(t *testing.T) {
	original := runPreDecision
	defer func() { runPreDecision = original }()
	for _, tool := range []string{"codex", "codex_start", "codex_apply"} {
		root, owner, e := mcpFixture(t, tool, tool)
		if tool == "codex_apply" {
			delete(e, "agent_id")
			e["tool_input"] = map[string]any{"jobId": "job", "acceptOnly": true}
		}
		runPreDecision = func(map[string]any, string, map[string]any) (map[string]any, error) {
			return nil, errors.New("injected preparation failure")
		}
		if err := PublishMCPHandoff(root, owner, e); err != nil {
			t.Fatal(err)
		}
		h, _ := PrepareMCP(root, owner, tool, stringValue(e["tool_name"]), e["tool_input"].(map[string]any))
		if h == nil {
			t.Fatal("no prepared failure")
		}
		out := h.Response["hookSpecificOutput"].(map[string]any)
		if tool == "codex" {
			if !strings.Contains(stringValue(out["additionalContext"]), "allowed unmodified") || !reflect.DeepEqual(h.Effective, h.Input) {
				t.Fatal(h.Response)
			}
		} else if out["permissionDecision"] != "deny" {
			t.Fatal("unsafe start/acceptance allowed", h.Response)
		}
	}
}

func TestMCPPreparationSuccessfulIdentityParity(t *testing.T) {
	root, owner, e := mcpFixture(t, "start-parity", "codex_start")
	repo, state := filepath.Join(root, "repo"), filepath.Join(root, "state")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := testutil.Hide(exec.Command("git", "init", "-q", repo)).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	plugin, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	t.Setenv("CLAUDE_PLUGIN_ROOT", plugin)
	t.Setenv("PURSER_STATE_DIR", state)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	e["hook_event_name"] = "PreToolUse"
	e["cwd"] = repo
	e["tool_input"] = map[string]any{"prompt": "ORCH|task=mcp-parity|phase=implementation|route=subtle|scope=src/a.go\nImplement the change.", "mode": "new", "timeoutSeconds": 90, "label": "parity"}
	want, err := Pre(e, state)
	if err != nil {
		t.Fatal(err)
	}
	output := mapValue(want["hookSpecificOutput"])
	if mapValue(output["updatedInput"]) == nil || output["permissionDecision"] != nil {
		t.Fatalf("not admitted: %v", want)
	}
	pending, _ := pendingPath(state, e)
	before, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if err := PublishMCPHandoff(state, owner, e); err != nil {
		t.Fatal(err)
	}
	h, err := PrepareMCP(state, owner, "start-parity", stringValue(e["tool_name"]), e["tool_input"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	if !p65JSONEqual(h.Response, want) {
		t.Fatalf("effective input differs:\n%v\n%v", h.Response, want)
	}
	after, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	_ = json.Unmarshal(before, &a)
	_ = json.Unmarshal(after, &b)
	for _, key := range []string{"call_id", "job_id", "lease_owner", "claude_session_id", "tool_use_id"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			t.Fatalf("identity %s: %v != %v", key, a[key], b[key])
		}
	}
	if a["lease_owner"] == nil {
		t.Fatal("missing pending ownership")
	}
}
