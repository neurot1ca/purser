package waiter_test

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/jobs"
	waitercommand "purser-hook/internal/waiter"
)

//go:embed testdata/wait_jobs.json
var waitFixtures embed.FS

func fixtureJob(t *testing.T, name, id string) map[string]any {
	t.Helper()
	data, err := waitFixtures.ReadFile("testdata/wait_jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]map[string]any
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	job, ok := fixtures[name]
	if !ok {
		t.Fatalf("missing wait fixture %q", name)
	}
	job["jobId"] = id
	return job
}

func writeWaitJob(t *testing.T, root, name, id string, updates map[string]any) string {
	t.Helper()
	job := fixtureJob(t, name, id)
	for key, value := range updates {
		job[key] = value
	}
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".json")
	writeWaitRecord(t, path, job)
	return path
}

func writeWaitRecord(t *testing.T, path string, job map[string]any) {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readWaitJob(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func rewriteWaitJob(t *testing.T, path, state string, port, generation int, deadline time.Time) {
	t.Helper()
	if err := rewriteWaitJobFile(path, state, port, generation, deadline); err != nil {
		t.Fatal(err)
	}
}

func rewriteWaitJobFile(path, state string, port, generation int, deadline time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var job map[string]any
	if err := json.Unmarshal(data, &job); err != nil {
		return err
	}
	job["state"] = state
	job["signalPort"] = nil
	if port != 0 {
		job["signalPort"] = port
	}
	job["generation"] = generation
	job["deadlineAt"] = float64(deadline.UnixNano()) / 1e9
	data, err = json.Marshal(job)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func requireWaitResult(t *testing.T, got jobs.WaitResult, code int, lines ...string) {
	t.Helper()
	if got.ExitCode != code || strings.Join(got.Lines, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("Wait result = %#v, want code %d and lines %#v", got, code, lines)
	}
}

func goStallFollowup(id string, minutes int) string {
	receipt := fmt.Sprintf("> ⚓ **Purser has seen no worker activity for %d minutes.** The job is still running; I'm checking its status.", minutes)
	return "Do not call codex_result yet. Restart " + waitercommand.Command(id) + " in the background (use the PowerShell tool on Windows); continue waiting. Tell the user in the user's language exactly one line, translated from: \"" + receipt + "\" Do not show the jobId to the user."
}

func TestTerminalJobReturnsImmediately(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "terminal_success", "terminal", nil)

	got := jobs.Wait(root, "terminal")
	record := readWaitJob(t, path)
	requireWaitResult(t, got, 0, "purser-wait terminal SUCCEEDED")
	if record["waiterObservedTerminalState"] != "SUCCEEDED" {
		t.Fatalf("terminal observation state = %#v", record["waiterObservedTerminalState"])
	}
	if _, ok := record["waiterObservedTerminalAt"].(float64); !ok {
		t.Fatalf("terminal observation timestamp = %#v", record["waiterObservedTerminalAt"])
	}
}

func TestValidationCommandQuotesArgumentsForCurrentShell(t *testing.T) {
	command := waitercommand.ValidationCommand(`C:\state dir\it's`, strings.Repeat("a", 64), strings.Repeat("b", 64))
	if !strings.Contains(command, "wait-validation") || !strings.Contains(command, strings.Repeat("a", 64)) || !strings.Contains(command, strings.Repeat("b", 64)) {
		t.Fatalf("ValidationCommand = %q", command)
	}
	if runtime.GOOS == "windows" && !strings.HasPrefix(command, "& '") {
		t.Fatalf("PowerShell command is not invokable: %q", command)
	}
	if runtime.GOOS != "windows" && !strings.HasPrefix(command, "'") {
		t.Fatalf("shell command is not quoted: %q", command)
	}
}

func TestStalledJobSurfacesOnceAndRerunWaitsUntilTerminal(t *testing.T) {
	root := t.TempDir()
	lastActivity := time.Now().Add(-10 * time.Minute)
	path := writeWaitJob(t, root, "stalled", "stalled", map[string]any{
		"deadlineAt":      float64(time.Now().Add(-time.Second).UnixNano()) / 1e9,
		"liveness":        map[string]any{"lastActivityAt": float64(lastActivity.UnixNano()) / 1e9},
		"stallDetectedAt": 1234.0,
	})

	first := jobs.Wait(root, "stalled")
	if first.ExitCode != 4 || len(first.Lines) != 2 {
		t.Fatalf("first Wait result = %#v", first)
	}
	if readWaitJob(t, path)["waiterObservedStallAt"] != 1234.0 {
		t.Fatalf("stall observation was not recorded: %#v", readWaitJob(t, path))
	}
	minutes := int(time.Since(lastActivity) / time.Minute)
	if first.Lines[0] != "purser-wait stalled STALLED (worker still running)" || first.Lines[1] != goStallFollowup("stalled", minutes) {
		t.Fatalf("first stall lines = %#v", first.Lines)
	}

	rewriteWaitJob(t, path, "RUNNING", 0, 1, time.Now().Add(5*time.Second))
	job := readWaitJob(t, path)
	job["errorCode"] = "STALLED"
	job["stallDetectedAt"] = 1234.0
	job["waiterObservedStallAt"] = 1234.0
	job["liveness"] = map[string]any{"lastActivityAt": float64(lastActivity.UnixNano()) / 1e9}
	writeWaitRecord(t, path, job)
	go func() {
		time.Sleep(30 * time.Millisecond)
		if err := rewriteWaitJobFile(path, "SUCCEEDED", 0, 1, time.Now().Add(time.Second)); err != nil {
			panic(err)
		}
	}()

	second := jobs.Wait(root, "stalled")
	requireWaitResult(t, second, 0, "purser-wait stalled SUCCEEDED")
}

func TestNewStallEpisodeSurfacesAgain(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "stalled", "stalled", map[string]any{
		"stallDetectedAt":       1235.0,
		"waiterObservedStallAt": 1234.0,
	})

	got := jobs.Wait(root, "stalled")
	requireWaitResult(t, got, 4,
		"purser-wait stalled STALLED (worker still running)",
		goStallFollowup("stalled", 0))
	if readWaitJob(t, path)["waiterObservedStallAt"] != 1235.0 {
		t.Fatalf("new stall episode was not recorded: %#v", readWaitJob(t, path))
	}
}

func TestTerminalFailedJobIsNonSuccess(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "terminal_failed", "failed", nil)

	got := jobs.Wait(root, "failed")
	requireWaitResult(t, got, 1, "purser-wait failed FAILED")
	if readWaitJob(t, path)["waiterObservedTerminalState"] != "FAILED" {
		t.Fatalf("failed terminal observation = %#v", readWaitJob(t, path)["waiterObservedTerminalState"])
	}
}

func TestTerminalObservationWriteFailurePreservesWaiterResult(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "terminal_failed", "failed", nil)
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), "failed.owner-lock"), 0700); err != nil {
		t.Fatal(err)
	}

	got := jobs.Wait(root, "failed")
	requireWaitResult(t, got, 1, "purser-wait failed FAILED")
	if _, ok := readWaitJob(t, path)["waiterObservedTerminalAt"]; ok {
		t.Fatalf("terminal observation unexpectedly persisted: %#v", readWaitJob(t, path))
	}
}

func TestJobRecordReadRetriesTransientPermissionError(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "terminal_success", "retry", nil)
	release := holdJobRead(t, path)
	released := make(chan struct{})
	go func() {
		time.Sleep(60 * time.Millisecond)
		release()
		close(released)
	}()

	started := time.Now()
	got := jobs.Wait(root, "retry")
	<-released
	requireWaitResult(t, got, 0, "purser-wait retry SUCCEEDED")
	if time.Since(started) < 40*time.Millisecond {
		t.Fatalf("waiter did not visibly retry the transient read")
	}
}

func TestRunningJobWaitsForListenerSignal(t *testing.T) {
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	path := writeWaitJob(t, root, "running", "running", map[string]any{"signalPort": port, "deadlineAt": float64(time.Now().Add(3*time.Second).UnixNano()) / 1e9})
	received := make(chan []byte, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		payload, _ := io.ReadAll(connection)
		received <- payload
		if err := rewriteWaitJobFile(path, "SUCCEEDED", port, 1, time.Now().Add(time.Second)); err != nil {
			panic(err)
		}
		_, _ = connection.Write([]byte("1"))
	}()

	got := jobs.Wait(root, "running")
	select {
	case payload := <-received:
		if string(payload) != "running:secret" {
			t.Fatalf("signal payload = %q", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not receive the signal")
	}
	requireWaitResult(t, got, 0, "purser-wait running SUCCEEDED")
}

func TestDeadHostReportsGoRecoveryState(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "running", "closed", map[string]any{
		"hostPid":        os.Getpid(),
		"hostCreatedAt":  0,
		"hostContained":  true,
		"pid":            os.Getpid() + 1,
		"generation":     1,
		"ownershipToken": "owner",
		"deadlineAt":     float64(time.Now().Add(time.Hour).UnixNano()) / 1e9,
	})

	got := jobs.Wait(root, "closed")
	requireWaitResult(t, got, 1, "purser-wait closed FAILED")
	job := readWaitJob(t, path)
	if job["state"] != "FAILED" || job["errorCode"] != "HOST_DIED" {
		t.Fatalf("dead-host recovery record = %#v", job)
	}
}

func TestWaitTimeoutReturnsRecordedRunningState(t *testing.T) {
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	writeWaitJob(t, root, "running", "timeout", map[string]any{"signalPort": port, "deadlineAt": float64(time.Now().Add(120*time.Millisecond).UnixNano()) / 1e9})
	received := make(chan []byte, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		payload, _ := io.ReadAll(connection)
		received <- payload
		time.Sleep(200 * time.Millisecond)
	}()

	got := jobs.Wait(root, "timeout")
	select {
	case payload := <-received:
		if string(payload) != "timeout:secret" {
			t.Fatalf("timeout signal payload = %q", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout listener did not receive the signal")
	}
	requireWaitResult(t, got, 3, "purser-wait timeout RUNNING")
}

func TestWaitFollowsReplacementGenerationEndpointAndDeadline(t *testing.T) {
	root := t.TempDir()
	oldListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer oldListener.Close()
	replacement, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	oldPort := oldListener.Addr().(*net.TCPAddr).Port
	newPort := replacement.Addr().(*net.TCPAddr).Port
	path := writeWaitJob(t, root, "running", "replaced", map[string]any{"signalPort": oldPort, "generation": 1, "deadlineAt": float64(time.Now().Add(time.Second).UnixNano()) / 1e9})
	received := make(chan []byte, 2)
	go func() {
		connection, acceptErr := oldListener.Accept()
		if acceptErr != nil {
			return
		}
		payload, _ := io.ReadAll(connection)
		received <- payload
		if err := rewriteWaitJobFile(path, "QUEUED", newPort, 2, time.Now().Add(2*time.Second)); err != nil {
			panic(err)
		}
		connection.Close()
	}()
	go func() {
		connection, acceptErr := replacement.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		payload, _ := io.ReadAll(connection)
		received <- payload
		if err := rewriteWaitJobFile(path, "RUNNING", newPort, 2, time.Now().Add(2*time.Second)); err != nil {
			panic(err)
		}
		time.Sleep(250 * time.Millisecond)
		if err := rewriteWaitJobFile(path, "SUCCEEDED", newPort, 2, time.Now().Add(time.Second)); err != nil {
			panic(err)
		}
		_, _ = connection.Write([]byte("1"))
	}()

	got := jobs.Wait(root, "replaced")
	for i := 0; i < 2; i++ {
		select {
		case payload := <-received:
			if string(payload) != "replaced:secret" {
				t.Fatalf("replacement signal payload = %q", payload)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("replacement listener did not receive both signals")
		}
	}
	requireWaitResult(t, got, 0, "purser-wait replaced SUCCEEDED")
}

func TestUnknownJobReturnsTwoAndOneLine(t *testing.T) {
	got := jobs.Wait(t.TempDir(), "unknown")
	requireWaitResult(t, got, 2, "purser-wait unknown UNKNOWN_JOB")
}

func TestGoWaiterMatchesGoHostTerminalAndStall(t *testing.T) {
	root := t.TempDir()
	path := writeWaitJob(t, root, "terminal_success", "gohosted", nil)

	terminal := jobs.Wait(root, "gohosted")
	requireWaitResult(t, terminal, 0, "purser-wait gohosted SUCCEEDED")
	if readWaitJob(t, path)["waiterObservedTerminalState"] != "SUCCEEDED" {
		t.Fatalf("Go-host terminal observation = %#v", readWaitJob(t, path))
	}

	job := readWaitJob(t, path)
	job["state"] = "RUNNING"
	job["errorCode"] = "STALLED"
	job["stallDetectedAt"] = 1234.0
	job["waiterObservedStallAt"] = nil
	job["signalPort"] = nil
	job["deadlineAt"] = float64(time.Now().Add(time.Minute).UnixNano()) / 1e9
	writeWaitRecord(t, path, job)

	stall := jobs.Wait(root, "gohosted")
	requireWaitResult(t, stall, 4,
		"purser-wait gohosted STALLED (worker still running)",
		goStallFollowup("gohosted", 0))
	if readWaitJob(t, path)["waiterObservedStallAt"] != 1234.0 {
		t.Fatalf("Go-host stall observation = %#v", readWaitJob(t, path))
	}
}
