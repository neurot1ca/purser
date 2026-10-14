package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/proc"
	"purser-hook/internal/waiter"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// One subprocess entry point for owner, bridge, and waiter fixtures.
func TestLifetimeHelper(t *testing.T) {
	switch os.Getenv("PURSER_LIFETIME_HELPER") {
	case "owner":
		fmt.Println("ready")
		time.Sleep(time.Minute)
		os.Exit(0)
	case "bridge":
		os.Exit(mcpMainWithStartup(nil, os.Stdin, os.Stdout, os.Stderr, func(ctx context.Context, _ io.Writer, stderr io.Writer) {
			<-ctx.Done()
			fmt.Fprintln(stderr, "maintenance cancelled")
		}))
	case "wait":
		t.Setenv("PURSER_STATE_DIR", os.Getenv("PURSER_LIFETIME_ROOT"))
		original := registerSessionOwner
		registerSessionOwner = func(ctx context.Context, owner waiter.Owner, done chan struct{}, stderr io.Writer) error {
			err := original(ctx, owner, done, stderr)
			if err == nil {
				fmt.Println("ready")
			}
			return err
		}
		os.Exit(waitMain([]string{"durable", "--session-owner", os.Getenv("PURSER_SESSION_OWNER")}))
	case "thread-parent":
		owner, err := waiter.PinOwner(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		type child struct {
			command *exec.Cmd
			input   io.WriteCloser
			output  *bufio.Reader
		}
		launched := make(chan child)
		release := make(chan struct{})
		go func() {
			runtime.LockOSThread() // Goexit retires this creating OS thread.
			command := proc.Hide(exec.Command(os.Args[0], "-test.run=^TestLifetimeHelper$"))
			command.Env = append(os.Environ(), "PURSER_LIFETIME_HELPER=bridge", "PURSER_SESSION_OWNER="+owner.String())
			input, err := command.StdinPipe()
			if err != nil {
				panic(err)
			}
			output, err := command.StdoutPipe()
			if err != nil {
				panic(err)
			}
			command.Stderr = os.Stderr
			if err = command.Start(); err != nil {
				panic(err)
			}
			launched <- child{command, input, bufio.NewReader(output)}
			<-release
			runtime.Goexit()
		}()
		process := <-launched
		for i := 0; i < 2; i++ {
			if _, err = fmt.Fprintln(process.input, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); err != nil {
				t.Fatal(err)
			}
			if _, err = process.output.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				close(release)
				time.Sleep(200 * time.Millisecond)
			}
		}
		process.input.Close()
		if err = process.command.Wait(); err != nil {
			t.Fatal(err)
		}
		fmt.Println("owner remained alive")
		os.Exit(0)
	}
}

func TestOwnerLifetimeCreatingThreadExit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux parent-death signals track the creating thread")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := proc.Hide(exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifetimeHelper$"))
	command.Env = append(os.Environ(), "PURSER_LIFETIME_HELPER=thread-parent", "PURSER_SESSION_OWNER=")
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "owner remained alive") {
		t.Fatalf("creating-thread exit cancelled live owner: %v %s", err, output)
	}
}

func TestOwnerLifetimeMismatchAndDenial(t *testing.T) {
	owner, err := waiter.PinOwner(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	owner.Created++
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	done, err := monitorSessionOwner(ctx, owner, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("identity mismatch not detected")
	}
	original := registerSessionOwner
	t.Cleanup(func() { registerSessionOwner = original })
	registerSessionOwner = func(context.Context, waiter.Owner, chan struct{}, io.Writer) error { return syscall.EACCES }
	owner.Created--
	_, err = monitorSessionOwner(ctx, owner, &stderr)
	if err == nil {
		t.Fatalf("denial not reported: %v", err)
	}
}

func TestOwnerLifetimeOpenInputAndDurableSurvival(t *testing.T) {
	executable, _ := os.Executable()
	start := func(mode, token string) (*exec.Cmd, *bytes.Buffer, io.WriteCloser, *bufio.Reader) {
		cmd := proc.Hide(exec.Command(executable, "-test.run=^TestLifetimeHelper$"))
		cmd.Env = append(os.Environ(), "PURSER_LIFETIME_HELPER="+mode, "PURSER_SESSION_OWNER="+token)
		output := &bytes.Buffer{}
		cmd.Stderr = output
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { input.Close() })
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() })
		return cmd, output, input, bufio.NewReader(stdout)
	}
	owner, _, _, ownerOutput := start("owner", "")
	if _, err := ownerOutput.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	pinned, err := waiter.PinOwner(owner.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	durable, _, _, durableOutput := start("owner", "")
	if _, err := durableOutput.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	scratch, err := filepath.Abs("../../../.tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(scratch, "lifetime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("PURSER_LIFETIME_ROOT", root)
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	durableIdentity, err := waiter.PinOwner(durable.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	record := []byte(fmt.Sprintf(`{"jobId":"durable","state":"RUNNING","hostPid":%d,"hostCreatedAt":%d,"hostContained":true,"signalToken":"token","deadlineAt":%d}`, durableIdentity.PID, durableIdentity.Created, time.Now().Add(time.Minute).Unix()))
	path := filepath.Join(dir, "durable.json")
	if err = os.WriteFile(path, record, 0600); err != nil {
		t.Fatal(err)
	}
	waiting, waitErrors, _, waitOutput := start("wait", pinned.String())
	if _, err := waitOutput.ReadString('\n'); err != nil {
		t.Fatalf("waiter registration: %v", err)
	}
	bridge, output, input, protocol := start("bridge", pinned.String())
	// Wait for registration and protocol admission before killing the owner.
	if _, err = io.WriteString(input, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n"); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := protocol.ReadString('\n')
		if err == nil && !strings.Contains(line, `"result":{}`) {
			err = fmt.Errorf("invalid protocol: %s", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge registration did not admit protocol")
	}
	owner.Process.Kill()
	owner.Wait()
	exited := make(chan error, 1)
	go func() { exited <- bridge.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("bridge: %v %s", err, output.String())
		}
		if !strings.Contains(output.String(), "maintenance cancelled") {
			t.Fatalf("maintenance not cancelled: %s", output.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner death did not stop bridge with open stdin")
	}
	waiterExit := make(chan error, 1)
	go func() { waiterExit <- waiting.Wait() }()
	select {
	case err := <-waiterExit:
		if err != nil {
			t.Fatalf("waiter: %v %s", err, waitErrors.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not stop")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, record) {
		t.Fatalf("durable record changed: %s %v", after, err)
	}
	if _, err := waiter.PinOwner(durable.Process.Pid); err != nil {
		t.Fatalf("durable stopped: %v", err)
	}
	durable.Process.Kill()
	durable.Wait()

}

func TestOwnerLifetimeRegistrationRace(t *testing.T) {
	cmd := proc.Hide(exec.Command(os.Args[0], "-test.run=^TestLifetimeHelper$"))
	cmd.Env = append(os.Environ(), "PURSER_LIFETIME_HELPER=owner")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	owner, err := waiter.PinOwner(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	original := registerSessionOwner
	defer func() { registerSessionOwner = original }()
	registerSessionOwner = func(context.Context, waiter.Owner, chan struct{}, io.Writer) error {
		cmd.Process.Kill()
		cmd.Wait()
		return nil
	}
	done, err := monitorSessionOwner(context.Background(), owner, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registration race missed")
	}
}
