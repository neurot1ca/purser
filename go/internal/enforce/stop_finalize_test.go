package enforce

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const stopLockHelperEnv = "PURSER_STOP_FINALIZE_LOCK_HELPER"

func TestStopFinalizeLockHelper(t *testing.T) {
	if os.Getenv(stopLockHelperEnv) != "1" {
		return
	}
	root := os.Getenv("PURSER_STOP_FINALIZE_ROOT")
	lock, ok := acquireTaskUsageLock(root, "p128-lock-contention", time.Second)
	if !ok {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("READY\n")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	releaseTaskUsageLock(lock)
	os.Exit(0)
}

func TestStopFinalizeP128Fixture(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "stop_finalize.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now   string `json:"now"`
		Cases []struct {
			Name       string          `json:"name"`
			Session    string          `json:"session_id"`
			Record     json.RawMessage `json:"record"`
			RawRecord  bool            `json:"raw_record"`
			Contention bool            `json:"lock_contention"`
			Expected   json.RawMessage `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339Nano, fixture.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			if len(c.Record) != 0 && string(c.Record) != "null" {
				path := activeTaskPath(root, c.Session)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				record := c.Record
				if c.RawRecord {
					var raw string
					if err := json.Unmarshal(c.Record, &raw); err != nil {
						t.Fatal(err)
					}
					record = []byte(raw)
				}
			if err := os.WriteFile(path, record, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if c.Contention {
				cmd := exec.Command(os.Args[0], "-test.run=^TestStopFinalizeLockHelper$")
				cmd.Env = append(os.Environ(), stopLockHelperEnv+"=1", "PURSER_STOP_FINALIZE_ROOT="+root)
				stdin, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				ready, err := bufio.NewReader(stdout).ReadString('\n')
				if err != nil || strings.TrimSpace(ready) != "READY" {
					_ = cmd.Process.Kill()
					t.Fatalf("lock helper readiness %q: %v", ready, err)
				}
				got := finalizeStopTaskDecision(root, c.Session, 20*time.Millisecond, now)
				_, _ = stdin.Write([]byte{'x'})
				_ = stdin.Close()
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
				assertStopFinalizeResult(t, got, c.Expected)
				return
			}
			got := finalizeStopTaskDecision(root, c.Session, 20*time.Millisecond, now)
			assertStopFinalizeResult(t, got, c.Expected)
		})
	}
}

func assertStopFinalizeResult(t *testing.T, got map[string]any, expected json.RawMessage) {
	t.Helper()
	actual, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(expected, &want); err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(wantJSON) {
		t.Fatalf("result %s; want %s", actual, wantJSON)
	}
}
