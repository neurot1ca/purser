package jobs

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAwaitHostPythonAcknowledgementOutcomes(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const token = "signal-token"
	t.Run("queued host acknowledges", func(t *testing.T) {
		stateRoot := t.TempDir()
		terminal := make(chan struct{})
		host, err := StartSignalHost(id, token, terminal, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer host.Close()
		writeAwaitRecord(t, stateRoot, map[string]any{"jobId": id, "state": "QUEUED", "leaseQueued": true,
			"signalPort": host.Addr().(*net.TCPAddr).Port, "signalToken": token, "acceptedAt": 10, "deadlineAt": 20})
		got := awaitHost(stateRoot, id, time.Second)
		if got["isError"] == true || got["structuredContent"].(map[string]any)["state"] != "QUEUED" {
			t.Fatalf("host acknowledgement not returned: %#v", got)
		}
	})
	t.Run("immediate completion is acknowledged as running", func(t *testing.T) {
		stateRoot := t.TempDir()
		writeAwaitRecord(t, stateRoot, map[string]any{"jobId": id, "state": "SUCCEEDED", "acceptedAt": 10, "deadlineAt": 20})
		got := awaitHost(stateRoot, id, time.Second)
		value := got["structuredContent"].(map[string]any)
		text := got["content"].([]any)[0].(map[string]any)["text"]
		if value["state"] != "RUNNING" || text != `{"jobId": "`+id+`", "acceptedAt": 10, "deadlineAt": 20, "state": "RUNNING"}` {
			t.Fatalf("immediate completion differs from Python: %#v", got)
		}
	})
	t.Run("running host process is acknowledged without signal ping", func(t *testing.T) {
		stateRoot := t.TempDir()
		created, ok := hostProcessIdentity()
		if !ok {
			t.Skip("current process identity is unavailable")
		}
		writeAwaitRecord(t, stateRoot, map[string]any{"jobId": id, "state": "RUNNING",
			"hostPid": os.Getpid(), "hostCreatedAt": created, "hostContained": runtime.GOOS == "windows",
			"acceptedAt": 10, "deadlineAt": 20})
		got := awaitHost(stateRoot, id, time.Second)
		if got["isError"] == true || got["structuredContent"].(map[string]any)["state"] != "RUNNING" {
			t.Fatalf("live host process was not acknowledged: %#v", got)
		}
	})
	if runtime.GOOS == "darwin" {
		t.Run("Darwin PID-only host is unconfirmed", func(t *testing.T) {
			stateRoot := t.TempDir()
			writeAwaitRecord(t, stateRoot, map[string]any{"jobId": id, "state": "RUNNING",
				"hostPid": os.Getpid(), "acceptedAt": 10, "deadlineAt": 20})
			got := awaitHost(stateRoot, id, time.Millisecond)
			value := got["structuredContent"].(map[string]any)
			if value["errorCode"] != "PROCESS_UNCONFIRMED" || value["message"] != "Host ownership was not acknowledged" {
				t.Fatalf("PID-only host was acknowledged: %#v", got)
			}
		})
	}
	t.Run("explicit unconfirmed state returns immediately", func(t *testing.T) {
		stateRoot := t.TempDir()
		writeAwaitRecord(t, stateRoot, map[string]any{"jobId": id, "state": "UNKNOWN_OR_IN_PROGRESS"})
		got := awaitHost(stateRoot, id, time.Second)
		value := got["structuredContent"].(map[string]any)
		if value["errorCode"] != "PROCESS_UNCONFIRMED" || value["message"] != "Host ownership or child state unconfirmed" {
			t.Fatalf("unconfirmed host result differs from Python: %#v", got)
		}
	})
	t.Run("missing acknowledgement times out", func(t *testing.T) {
		stateRoot := t.TempDir()
		writeAwaitRecord(t, stateRoot, map[string]any{"jobId": id, "state": "QUEUED", "acceptedAt": 10, "deadlineAt": 20})
		got := awaitHost(stateRoot, id, time.Millisecond)
		value := got["structuredContent"].(map[string]any)
		if value["errorCode"] != "PROCESS_UNCONFIRMED" || value["message"] != "Host ownership was not acknowledged" {
			t.Fatalf("timeout result differs from Python: %#v", got)
		}
	})
}

func writeAwaitRecord(t *testing.T, root string, record map[string]any) {
	t.Helper()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, record["jobId"].(string)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
