package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSupervisionHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "supervisor" {
			os.Exit(SupervisorMain(os.Args[i+1:]))
		}
		if arg == "worker" {
			mode, path := os.Args[i+1], os.Args[i+2]
			if mode == "owner" {
				s, err := StartSupervised(context.Background(), os.Args[0], []string{"-test.run=TestSupervisionHelper", "--", "supervisor"}, Launch{Args: []string{os.Args[0], "-test.run=TestSupervisionHelper", "--", "worker", "wait", path}})
				if err != nil {
					panic(err)
				}
				data, _ := json.Marshal(s.GuardPID)
				if err := os.WriteFile(path+".guard", data, 0600); err != nil {
					panic(err)
				}
				time.Sleep(time.Hour)
				return
			}
			if mode != "leaf" {
				fmt.Fprintln(os.Stdout, "worker stdout")
				fmt.Fprintln(os.Stderr, "worker stderr")
				c := Hide(exec.Command(os.Args[0], "-test.run=TestSupervisionHelper", "--", "worker", "leaf", path))
				c.Stdout, c.Stderr = os.Stdout, os.Stderr
				if err := c.Start(); err != nil {
					panic(err)
				}
				data, _ := json.Marshal(c.Process.Pid)
				if err := os.WriteFile(path, data, 0600); err != nil {
					panic(err)
				}
				if mode == "exit" {
					return
				}
			}
			time.Sleep(time.Hour)
			return
		}
	}
}

func TestSupervisionLifecycle(t *testing.T) {
	for _, mode := range []string{"exit", "cancel", "owner", "missing"} {
		t.Run(mode, func(t *testing.T) {
			// Keep subprocess scratch in the repository's ignored scratch directory.
			if err := os.MkdirAll("../../.tmp", 0700); err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp("../../.tmp", "supervision-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path, _ := filepath.Abs(filepath.Join(dir, "pid"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			workerMode := mode
			if mode == "cancel" {
				workerMode = "wait"
			}
			argv := []string{os.Args[0], "-test.run=TestSupervisionHelper", "--", "worker", workerMode, path}
			if mode == "missing" {
				argv = []string{filepath.Join(dir, "absent")}
			}
			if mode == "owner" {
				owner := Hide(exec.Command(argv[0], argv[1:]...))
				if err := owner.Start(); err != nil {
					t.Fatal(err)
				}
				defer owner.Process.Kill()
				pid := awaitLeaf(t, path)
				guard := awaitLeaf(t, path+".guard")
				if err := owner.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = owner.Wait()
				awaitStopped(t, pid)
				awaitStopped(t, guard)
				return
			}
			stdout, outWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			defer outWriter.Close()
			stderr, errWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			defer errWriter.Close()
			outDone, errDone := make(chan string, 1), make(chan string, 1)
			go func() { data, _ := io.ReadAll(stdout); outDone <- string(data) }()
			go func() { data, _ := io.ReadAll(stderr); errDone <- string(data) }()
			s, err := StartSupervised(ctx, os.Args[0], []string{"-test.run=TestSupervisionHelper", "--", "supervisor"}, Launch{Args: argv, Stdout: outWriter, Stderr: errWriter})
			_ = outWriter.Close()
			_ = errWriter.Close()
			if err != nil {
				t.Fatal(err)
			}
			pid := 0
			if mode != "missing" {
				pid = awaitLeaf(t, path)
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case <-s.Done():
				result := s.Result()
				if !result.Confirmed {
					t.Fatalf("unconfirmed: %+v", result)
				}
				if mode == "missing" && result.Err == nil {
					t.Fatal("launch error lost")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("supervision exceeded teardown bound")
			}
			if pid != 0 {
				awaitStopped(t, pid)
			}
			for i, done := range []chan string{outDone, errDone} {
				select {
				case output := <-done:
					if mode != "missing" {
						want := "worker stdout"
						other := "worker stderr"
						if i == 1 {
							want, other = other, want
						}
						if !strings.Contains(output, want) || strings.Contains(output, other) {
							t.Fatalf("stream %d: %q", i, output)
						}
					}
				case <-time.After(time.Second):
					t.Fatal("descendant retained output pipe")
				}
			}
		})
	}
}

func awaitLeaf(t *testing.T, path string) int {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		data, err := os.ReadFile(path)
		var pid int
		if err == nil && json.Unmarshal(data, &pid) == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker was not admitted")
	return 0
}
func awaitStopped(t *testing.T, pid int) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if processStopped(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant %d survived", pid)
}
