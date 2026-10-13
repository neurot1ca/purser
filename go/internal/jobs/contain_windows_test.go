//go:build windows

package jobs

import "purser-hook/internal/testutil"

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestContainmentHelper(t *testing.T) {
	mode := os.Getenv("PURSER_CONTAIN_HELPER")
	if mode == "host" {
		job, err := ContainCurrentProcess()
		if err != nil {
			panic(err)
		}
		child := helper("root-wait")
		child.Stdout = os.Stdout
		childInput, err := child.StdinPipe()
		if err != nil {
			panic(err)
		}
		if err = child.Start(); err != nil {
			panic(err)
		}
		fmt.Printf("ROOT %d\n", child.Process.Pid)
		input := bufio.NewScanner(os.Stdin)
		for input.Scan() {
			if input.Text() == "spawn" {
				_, _ = fmt.Fprintln(childInput, "spawn")
			}
			if input.Text() == "kill-root" {
				_ = child.Process.Kill()
				_ = child.Wait()
				fmt.Printf("CONFIRMED %t\n", job.ConfirmDescendantsStopped(true))
			}
		}
		return
	}
	if mode == "root-wait" {
		input := bufio.NewScanner(os.Stdin)
		for input.Scan() {
			if input.Text() == "spawn" {
				grandchild := helper("grandchild")
				if err := grandchild.Start(); err != nil {
					panic(err)
				}
				fmt.Printf("GRAND %d\n", grandchild.Process.Pid)
				_ = grandchild.Wait()
			}
		}
		return
	}
	if mode == "grandchild" {
		time.Sleep(time.Hour)
		return
	}
	t.Run("terminate job kills child tree and confirms", func(t *testing.T) {
		root := helper("root-wait")
		stdout := stdoutPipe(t, root)
		stdin := stdinPipe(t, root)
		check(t, root.Start())
		reader := bufio.NewScanner(stdout)
		job, err := NewProcessJob()
		check(t, err)
		defer job.Close()
		check(t, job.Assign(root.Process.Pid))
		_, _ = fmt.Fprintln(stdin, "spawn")
		grandWait := openProcess(t, readPID(t, reader, "GRAND"))
		defer syscall.CloseHandle(grandWait)
		check(t, root.Process.Kill())
		_ = root.Wait()
		if job.ConfirmDescendantsStopped(true) {
			t.Fatal("outside owner incorrectly discounted the surviving descendant")
		}
		check(t, job.Terminate(1))
		if !job.ConfirmDescendantsStopped(true) {
			t.Fatal("terminated job was not confirmed empty")
		}
		waitExited(t, grandWait)
	})
	t.Run("host death kills descendants and surviving descendant is unconfirmed", func(t *testing.T) {
		host, reader, stdin, _ := startHost(t)
		_, _ = fmt.Fprintln(stdin, "spawn")
		grandWait := openProcess(t, readPID(t, reader, "GRAND"))
		defer syscall.CloseHandle(grandWait)
		_, _ = fmt.Fprintln(stdin, "kill-root")
		for reader.Scan() && reader.Text() != "CONFIRMED false" && reader.Text() != "CONFIRMED true" {
		}
		if reader.Text() != "CONFIRMED false" {
			t.Fatalf("surviving descendant confirmation = %q", reader.Text())
		}
		_ = host.Process.Kill()
		_ = host.Wait()
		waitExited(t, grandWait)
	})
	t.Run("forced host death kills descendants", func(t *testing.T) {
		host, reader, stdin, rootPID := startHost(t)
		rootWait := openProcess(t, rootPID)
		defer syscall.CloseHandle(rootWait)
		_, _ = fmt.Fprintln(stdin, "spawn")
		grandWait := openProcess(t, readPID(t, reader, "GRAND"))
		defer syscall.CloseHandle(grandWait)
		_ = host.Process.Kill()
		_ = host.Wait()
		waitExited(t, rootWait)
		waitExited(t, grandWait)
	})
}
func startHost(t *testing.T) (*exec.Cmd, *bufio.Scanner, io.WriteCloser, int) {
	host := helper("host")
	stdout := stdoutPipe(t, host)
	stdin := stdinPipe(t, host)
	check(t, host.Start())
	reader := bufio.NewScanner(stdout)
	return host, reader, stdin, readPID(t, reader, "ROOT")
}
func helper(mode string) *exec.Cmd {
	cmd := testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestContainmentHelper$"))
	env := os.Environ()
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], "PURSER_CONTAIN_HELPER=") {
			env = append(env[:i], env[i+1:]...)
		}
	}
	cmd.Env = append(env, "PURSER_CONTAIN_HELPER="+mode)
	return cmd
}
func readPID(t *testing.T, scanner *bufio.Scanner, label string) int {
	for scanner.Scan() {
		var got string
		var pid int
		if _, err := fmt.Sscan(scanner.Text(), &got, &pid); err == nil && got == label {
			return pid
		}
	}
	t.Fatalf("missing %s process line", label)
	return 0
}
func stdoutPipe(t *testing.T, cmd *exec.Cmd) io.ReadCloser {
	pipe, err := cmd.StdoutPipe()
	check(t, err)
	return pipe
}
func stdinPipe(t *testing.T, cmd *exec.Cmd) io.WriteCloser {
	pipe, err := cmd.StdinPipe()
	check(t, err)
	return pipe
}
func check(t *testing.T, err error) {
	if err != nil {
		t.Fatal(err)
	}
}
func openProcess(t *testing.T, pid int) syscall.Handle {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}
	return h
}
func waitExited(t *testing.T, handle syscall.Handle) {
	result, err := syscall.WaitForSingleObject(handle, 5000)
	if err != nil || result != syscall.WAIT_OBJECT_0 {
		t.Fatalf("process exit wait = %d, %v", result, err)
	}
}
