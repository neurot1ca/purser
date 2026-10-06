//go:build windows

package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDetachedHelper(t *testing.T) {
	if os.Getenv("PURSER_DETACH_LAUNCHER") == "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		env := append(os.Environ(), "PURSER_DETACH_CHILD=1")
		pid, identity, err := LaunchDetached([]string{exe, "-test.run=^TestDetachedHelper$"}, env, "")
		if err != nil {
			if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
				fmt.Println("DETACH_UNAVAILABLE_ACCESS_DENIED")
				return
			}
			t.Fatal(err)
		}
		fmt.Printf("%d %d\n", pid, identity)
		return
	}
	if os.Getenv("PURSER_DETACH_CHILD") == "1" {
		consoleWindow, _, _ := syscall.NewLazyDLL("user32.dll").NewProc("GetConsoleWindow").Call()
		if consoleWindow != 0 {
			_ = os.WriteFile(os.Getenv("PURSER_DETACH_CONSOLE_MARKER"), nil, 0600)
		}
		for os.Getenv("PURSER_DETACH_RELEASE") == "" {
			time.Sleep(10 * time.Millisecond)
		}
		for {
			if _, err := os.Stat(os.Getenv("PURSER_DETACH_RELEASE")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestDetachSurvivesLauncherExitAndChecksCreationIdentity(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	consoleMarker := filepath.Join(dir, "console-window")
	parentPID := strconv.Itoa(os.Getpid())
	launcher := exec.Command(os.Args[0], "-test.run=^TestDetachedHelper$")
	launcher.Env = append(os.Environ(),
		"PURSER_DETACH_LAUNCHER=1",
		"PURSER_DETACH_RELEASE="+release,
		"PURSER_DETACH_CONSOLE_MARKER="+consoleMarker,
		"PURSER_CODEX_TEST_PARENT_PID="+parentPID,
	)
	output, err := launcher.CombinedOutput()
	if err != nil {
		t.Fatalf("launcher failed: %v: %s", err, output)
	}
	if strings.Contains(string(output), "DETACH_UNAVAILABLE_ACCESS_DENIED") {
		t.Skip("Windows job policy denied CREATE_BREAKAWAY_FROM_JOB")
	}
	fields := strings.Fields(strings.SplitN(string(output), "\n", 2)[0])
	if len(fields) != 2 {
		t.Fatalf("unexpected launcher result %q", output)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	identity, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	if !IdentityMatches(pid, identity) {
		t.Fatal("creation identity did not match the live detached process")
	}
	if _, err := os.Stat(consoleMarker); err == nil {
		t.Fatal("detached child has a console window")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "codex_exec_records", "running.json")
	fixtureData, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(fixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	var pythonRecord map[string]json.RawMessage
	for name, content := range fixture.Files {
		if strings.HasSuffix(name, ".json") {
			if err := json.Unmarshal([]byte(content), &pythonRecord); err == nil && pythonRecord["hostPid"] != nil {
				break
			}
		}
	}
	if pythonRecord["hostPid"] == nil || pythonRecord["hostCreatedAt"] == nil {
		t.Fatal("fixture is missing Python host identity keys")
	}
	goRecord, err := json.Marshal(struct {
		PID       int   `json:"hostPid"`
		CreatedAt int64 `json:"hostCreatedAt"`
	}{pid, identity})
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]json.RawMessage
	if err := json.Unmarshal(goRecord, &actual); err != nil {
		t.Fatal(err)
	}
	if len(actual) != 2 || actual["hostPid"] == nil || actual["hostCreatedAt"] == nil {
		t.Fatalf("Go host identity fields differ from Python record keys: %s", goRecord)
	}

	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && IdentityMatches(pid, identity) {
		time.Sleep(10 * time.Millisecond)
	}
	if IdentityMatches(pid, identity) {
		t.Fatal("creation identity still matched after the detached process exited")
	}
}
