package jobs

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/waiter"
)

func writeWaitJob(t *testing.T, root, id string, fields map[string]any) string {
	t.Helper()
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	job := map[string]any{"jobId": id, "state": "RUNNING", "signalPort": nil,
		"signalToken": "secret", "deadlineAt": float64(time.Now().Add(5*time.Second).UnixNano()) / 1e9}
	for key, value := range fields {
		job[key] = value
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readWaitFields(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestWaitInvalidAndMissingRecord(t *testing.T) {
	root := t.TempDir()
	if got := Wait(root, "missing"); got.ExitCode != 2 || strings.Join(got.Lines, "\n") != "purser-wait missing UNKNOWN_JOB" {
		t.Fatalf("missing result = %#v", got)
	}
	if got := Wait(root, "../missing"); got.ExitCode != 2 || got.Lines[0] != "purser-wait ../missing UNKNOWN_JOB" {
		t.Fatalf("invalid id result = %#v", got)
	}
	writeWaitJob(t, root, "mismatch", map[string]any{"jobId": "other"})
	if got := Wait(root, "mismatch"); got.ExitCode != 2 || got.Lines[0] != "purser-wait mismatch UNKNOWN_JOB" {
		t.Fatalf("mismatched record result = %#v", got)
	}
}

func TestWaitAlreadyTerminalWritesObservation(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "done", map[string]any{"state": "SUCCEEDED"})
	got := Wait(root, "done")
	fields := readWaitFields(t, path)
	if got.ExitCode != 0 || strings.Join(got.Lines, "\n") != "purser-wait done SUCCEEDED" ||
		fields["waiterObservedTerminalState"] != "SUCCEEDED" {
		t.Fatalf("result=%#v record=%#v", got, fields)
	}
	if _, ok := fields["waiterObservedTerminalAt"].(float64); !ok {
		t.Fatalf("terminal timestamp missing or invalid: %#v", fields["waiterObservedTerminalAt"])
	}
}

func TestWaitReturnsStillWaitingAtConfiguredBound(t *testing.T) {
	t.Setenv("PURSER_WAIT_MAX_SECONDS", "1")
	root := t.TempDir()
	writeWaitJob(t, root, "queued", map[string]any{
		"state":      "QUEUED",
		"deadlineAt": float64(time.Now().Add(time.Hour).UnixNano()) / 1e9,
	})

	started := time.Now()
	got := Wait(root, "queued")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Wait took %s, want configured bound within 2s", elapsed)
	}
	if got.ExitCode != 5 || len(got.Lines) != 1 || got.Lines[0] != "purser-wait queued STILL_WAITING" {
		t.Fatalf("Wait = %#v, want STILL_WAITING with exit code 5", got)
	}
}

func TestWaitDoesNotSleepAfterSignalExhaustsBound(t *testing.T) {
	t.Setenv("PURSER_WAIT_MAX_SECONDS", "1")
	terminal := make(chan struct{})
	host, err := StartSignalHost("bounded", "secret", terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	defer close(terminal)
	root := t.TempDir()
	writeWaitJob(t, root, "bounded", map[string]any{
		"signalPort": host.Addr().(*net.TCPAddr).Port,
		"deadlineAt": float64(time.Now().Add(time.Hour).UnixNano()) / 1e9,
	})
	previousSleep := waitSleep
	defer func() { waitSleep = previousSleep }()
	started := time.Now()
	waitSleep = func(duration time.Duration) {
		if time.Since(started) >= time.Second && duration > 0 {
			t.Errorf("Wait sleeps %s after its configured bound", duration)
		}
		time.Sleep(duration)
	}
	got := Wait(root, "bounded")
	if got.ExitCode != 5 || len(got.Lines) != 1 || got.Lines[0] != "purser-wait bounded STILL_WAITING" {
		t.Fatalf("Wait = %#v, want STILL_WAITING with exit code 5", got)
	}
}

func TestWaitObservesTerminalDuringSignalWait(t *testing.T) {
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	path := writeWaitJob(t, root, "during", map[string]any{"signalPort": port})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		buf := make([]byte, 64)
		_, _ = connection.Read(buf)
		_ = writeWaitRecord(path, "SUCCEEDED", port, 1, time.Now().Add(3*time.Second))
		_, _ = connection.Write([]byte("1"))
	}()
	got := Wait(root, "during")
	fields := readWaitFields(t, path)
	if got.ExitCode != 0 || got.Lines[0] != "purser-wait during SUCCEEDED" || fields["waiterObservedTerminalState"] != "SUCCEEDED" {
		t.Fatalf("result=%#v record=%#v", got, fields)
	}
}

func TestWaitFollowsEndpointGenerationChange(t *testing.T) {
	root := t.TempDir()
	oldListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer oldListener.Close()
	newListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer newListener.Close()
	oldPort := oldListener.Addr().(*net.TCPAddr).Port
	newPort := newListener.Addr().(*net.TCPAddr).Port
	path := writeWaitJob(t, root, "replace", map[string]any{"signalPort": oldPort, "generation": 1})
	go func() {
		connection, acceptErr := oldListener.Accept()
		if acceptErr != nil {
			return
		}
		buf := make([]byte, 64)
		_, _ = connection.Read(buf)
		_ = writeWaitRecord(path, "QUEUED", newPort, 2, time.Now().Add(3*time.Second))
		_ = connection.Close()
	}()
	go func() {
		connection, acceptErr := newListener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		buf := make([]byte, 64)
		_, _ = connection.Read(buf)
		time.Sleep(50 * time.Millisecond)
		_ = writeWaitRecord(path, "SUCCEEDED", newPort, 2, time.Now().Add(3*time.Second))
		_, _ = connection.Write([]byte("1"))
	}()
	got := Wait(root, "replace")
	if got.ExitCode != 0 || got.Lines[0] != "purser-wait replace SUCCEEDED" {
		t.Fatalf("result = %#v", got)
	}
}

func TestWaitRefusedSignalPortBacksOff(t *testing.T) {
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	writeWaitJob(t, root, "refused", map[string]any{
		"signalPort": port,
		"deadlineAt": float64(time.Now().Add(250*time.Millisecond).UnixNano()) / 1e9,
	})

	var sleeps int
	previousSleep := waitSleep
	waitSleep = func(duration time.Duration) {
		sleeps++
		time.Sleep(duration)
	}
	defer func() { waitSleep = previousSleep }()

	got := Wait(root, "refused")
	if got.ExitCode != 3 || got.Lines[0] != "purser-wait refused RUNNING" {
		t.Fatalf("result = %#v", got)
	}
	if sleeps == 0 || sleeps > 4 {
		t.Fatalf("backoff sleeps = %d, want 1..4", sleeps)
	}
}

func TestWaitReportsStallOnce(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "stalled", map[string]any{
		"errorCode": "STALLED", "stallDetectedAt": 1234.0,
		"deadlineAt": float64(time.Now().Add(5*time.Second).UnixNano()) / 1e9,
		"liveness":   map[string]any{"lastActivityAt": float64(time.Now().Add(-125*time.Second).UnixNano()) / 1e9},
	})
	first := Wait(root, "stalled")
	fields := readWaitFields(t, path)
	if first.ExitCode != 4 || len(first.Lines) != 2 || fields["waiterObservedStallAt"] != 1234.0 ||
		!strings.Contains(first.Lines[1], "Restart "+waiter.Command("stalled")+" in the background") ||
		!strings.Contains(first.Lines[1], "Do not show the jobId to the user.") {
		t.Fatalf("first result=%#v record=%#v", first, fields)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = writeWaitRecord(path, "SUCCEEDED", 0, 1, time.Now().Add(time.Second))
	}()
	second := Wait(root, "stalled")
	if second.ExitCode != 0 || len(second.Lines) != 1 || second.Lines[0] != "purser-wait stalled SUCCEEDED" {
		t.Fatalf("second result = %#v", second)
	}
}

func TestWaitExitCodes(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		id    string
		state string
		code  int
	}{
		{"success", "SUCCEEDED", 0}, {"failure", "FAILED", 1},
		{"running", "RUNNING", 3},
	} {
		fields := map[string]any{"state": test.state}
		if test.state == "RUNNING" {
			fields["deadlineAt"] = float64(time.Now().Add(-time.Second).UnixNano()) / 1e9
		}
		writeWaitJob(t, root, test.id, fields)
		got := Wait(root, test.id)
		if got.ExitCode != test.code || got.Lines[0] != "purser-wait "+test.id+" "+test.state {
			t.Errorf("Wait(%s) = %#v, want code %d", test.id, got, test.code)
		}
	}
	if got := Wait(root, "invalid"); got.ExitCode != 2 {
		t.Errorf("invalid record code = %d", got.ExitCode)
	}
}

func TestWaitFinishesDeadHostBeforeDeadline(t *testing.T) {
	t.Setenv("PURSER_WAIT_MAX_SECONDS", "1")
	root := t.TempDir()
	created, ok := hostProcessIdentity()
	if !ok {
		t.Fatal("test process identity unavailable")
	}
	createdAt, valid := created.(int64)
	if !valid {
		createdAt = int64(created.(uint64))
	}
	deadHost := os.Getpid()
	path := writeWaitJob(t, root, "deadhost", map[string]any{
		"hostPid": deadHost, "hostCreatedAt": createdAt + 1, "hostContained": true,
		"pid": deadHost + 1, "generation": 1, "ownershipToken": "owner",
		"deadlineAt": float64(time.Now().Add(time.Hour).UnixNano()) / 1e9,
	})
	got := Wait(root, "deadhost")
	fields := readWaitFields(t, path)
	if got.ExitCode != 1 || got.Lines[0] != "purser-wait deadhost FAILED" ||
		fields["state"] != "FAILED" || fields["errorCode"] != "HOST_DIED" {
		t.Fatalf("result=%#v record=%#v", got, fields)
	}
}

func TestWaitLiveHostBeforeDeadlineStillWaiting(t *testing.T) {
	t.Setenv("PURSER_WAIT_MAX_SECONDS", "1")
	created, ok := hostProcessIdentity()
	if !ok {
		t.Fatal("test process identity unavailable")
	}
	root := t.TempDir()
	path := writeWaitJob(t, root, "livehost", map[string]any{
		"hostPid": os.Getpid(), "hostCreatedAt": created, "hostContained": true,
		"deadlineAt": float64(time.Now().Add(time.Hour).UnixNano()) / 1e9,
	})
	got := Wait(root, "livehost")
	fields := readWaitFields(t, path)
	if got.ExitCode != 5 || len(got.Lines) != 1 || got.Lines[0] != "purser-wait livehost STILL_WAITING" || fields["state"] != "RUNNING" {
		t.Fatalf("result=%#v record=%#v", got, fields)
	}
}

func TestWaitUnknownHostLivenessBeforeDeadlineStillWaiting(t *testing.T) {
	t.Setenv("PURSER_WAIT_MAX_SECONDS", "1")
	root := t.TempDir()
	path := writeWaitJob(t, root, "unknownhost", map[string]any{
		"hostPid": os.Getpid(), "hostCreatedAt": "unknown", "hostContained": true,
		"deadlineAt": float64(time.Now().Add(time.Hour).UnixNano()) / 1e9,
	})
	got := Wait(root, "unknownhost")
	fields := readWaitFields(t, path)
	if got.ExitCode != 5 || len(got.Lines) != 1 || got.Lines[0] != "purser-wait unknownhost STILL_WAITING" || fields["state"] != "RUNNING" {
		t.Fatalf("result=%#v record=%#v", got, fields)
	}
}

func writeWaitRecord(path, state string, port, generation int, deadline time.Time) error {
	fields := readWaitFieldsForWrite(path)
	fields["state"] = state
	fields["generation"] = generation
	fields["signalPort"] = nil
	if port > 0 {
		fields["signalPort"] = port
	}
	fields["deadlineAt"] = float64(deadline.UnixNano()) / 1e9
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func readWaitFieldsForWrite(path string) map[string]any {
	data, _ := os.ReadFile(path)
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	return fields
}
