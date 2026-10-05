package jobs

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSignalProtocol(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.json")
	record, _ := Decode([]byte(`{"jobId":"job","state":"RUNNING"}`))
	if err := Write(path, record); err != nil {
		t.Fatal(err)
	}
	terminal := make(chan struct{})
	host, err := StartSignalHost("job", "secret", terminal, func() bool {
		current, err := Read(path)
		if err != nil {
			return false
		}
		if err := current.Set("state", "CANCELLED"); err != nil {
			return false
		}
		if err := Write(path, current); err != nil {
			return false
		}
		close(terminal)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	address := host.Addr().String()

	if ok, err := SignalPing(address, "job", "secret", time.Second); err != nil || !ok {
		t.Fatalf("valid PING: ok=%v err=%v", ok, err)
	}
	if ok, err := SignalPing(address, "job", "wrong", time.Second); err != nil || ok {
		t.Fatalf("wrong token PING: ok=%v err=%v", ok, err)
	}
	if ok, err := SignalCancel(address, "job", "secret", time.Second); err != nil || !ok {
		t.Fatalf("CANCEL: ok=%v err=%v", ok, err)
	}
	if ok, err := SignalWait(address, "job", "secret", time.Second); err != nil || !ok {
		t.Fatalf("wait: ok=%v err=%v", ok, err)
	}
	current, err := Read(path)
	var state string
	if err != nil || !current.Get("state", &state) || state != "CANCELLED" {
		t.Fatalf("wait acknowledged before durable terminal state: state=%q err=%v", state, err)
	}
}

func TestSignalWaitOutlivesWriteDeadline(t *testing.T) {
	previous := signalWriteTimeout
	signalWriteTimeout = 30 * time.Millisecond
	defer func() { signalWriteTimeout = previous }()
	terminal := make(chan struct{})
	host, err := StartSignalHost("job", "secret", terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go func() {
		time.Sleep(3 * signalWriteTimeout)
		close(terminal)
	}()
	if ok, err := SignalWait(host.Addr().String(), "job", "secret", time.Second); err != nil || !ok {
		t.Fatalf("wait after long-running job: ok=%v err=%v", ok, err)
	}
}

func TestSignalRejectsOversizedAndGarbledMessages(t *testing.T) {
	terminal := make(chan struct{})
	host, err := StartSignalHost("job", "secret", terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	for _, message := range []string{strings.Repeat("x", signalMessageLimit+1), "job:secret:NOT_A_COMMAND", "job:secret\xff:PING"} {
		connection, err := net.DialTimeout("tcp", host.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = connection.SetDeadline(time.Now().Add(time.Second))
		_, _ = connection.Write([]byte(message))
		var response [1]byte
		n, _ := connection.Read(response[:])
		_ = connection.Close()
		if n != 0 {
			t.Fatalf("accepted malformed signal %q", message)
		}
	}
}

// TestSignalHostProcess is also invoked directly by the Python interop test
// from the compiled test binary, keeping the Go host on a real process boundary.
func TestSignalHostProcess(t *testing.T) {
	if os.Getenv("PURSER_SIGNAL_HOST_HELPER") != "1" {
		return
	}
	jobID, token, path := os.Getenv("PURSER_SIGNAL_JOB"), os.Getenv("PURSER_SIGNAL_TOKEN"), os.Getenv("PURSER_SIGNAL_RECORD")
	terminal := make(chan struct{})
	var terminalMu sync.Mutex
	setTerminal := func(state string) bool {
		terminalMu.Lock()
		defer terminalMu.Unlock()
		select {
		case <-terminal:
			return false
		default:
		}
		current, err := Read(path)
		var currentState string
		if err != nil {
			return false
		}
		if current.Get("state", &currentState) && (currentState == "SUCCEEDED" || currentState == "FAILED" ||
			currentState == "CANCELLED" || currentState == "TIMED_OUT" || currentState == "UNKNOWN_OR_IN_PROGRESS") {
			close(terminal)
			return false
		}
		if current.Set("state", state) != nil || Write(path, current) != nil {
			return false
		}
		close(terminal)
		return true
	}
	host, err := StartSignalHost(jobID, token, terminal, func() bool { return setTerminal("CANCELLED") })
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	readyPath := os.Getenv("PURSER_SIGNAL_READY")
	if err := os.WriteFile(readyPath, []byte(fmt.Sprint(host.Addr().(*net.TCPAddr).Port)), 0600); err != nil {
		t.Fatal(err)
	}
	completion := time.AfterFunc(500*time.Millisecond, func() { _ = setTerminal("SUCCEEDED") })
	<-terminal
	completion.Stop()
}

func TestSignalClientProcess(t *testing.T) {
	if os.Getenv("PURSER_SIGNAL_CLIENT_HELPER") != "1" {
		return
	}
	address := net.JoinHostPort("127.0.0.1", os.Getenv("PURSER_SIGNAL_PORT"))
	jobID, token := os.Getenv("PURSER_SIGNAL_JOB"), os.Getenv("PURSER_SIGNAL_TOKEN")
	ping, pingErr := SignalPing(address, jobID, token, 2*time.Second)
	wrong, wrongErr := SignalPing(address, jobID, "wrong-token", 2*time.Second)
	waitResult := make(chan bool, 1)
	go func() {
		ok, _ := SignalWait(address, jobID, token, 10*time.Second)
		waitResult <- ok
	}()
	time.Sleep(100 * time.Millisecond)
	cancel, cancelErr := SignalCancel(address, jobID, token, 7*time.Second)
	wait := <-waitResult
	result, _ := json.Marshal(map[string]bool{"ping": ping && pingErr == nil, "wrongRejected": !wrong && wrongErr == nil,
		"cancel": cancel && cancelErr == nil, "wait": wait})
	fmt.Println(string(result))
}
