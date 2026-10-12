package ledger

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"purser-hook/internal/proc"
	"purser-hook/internal/profile"
)

const worktreeSetupOutputLimit = 2048

// WorktreeSetupError is the failure record written to an async job.
type WorktreeSetupError struct {
	ExitCode *int   `json:"exitCode"`
	TimedOut bool   `json:"timedOut"`
	Output   string `json:"output"`
}

// WorktreeSetupResult makes setup failure explicit so launch assembly can stop.
type WorktreeSetupResult struct {
	Failure *WorktreeSetupError `json:"setupError,omitempty"`
}

func (r WorktreeSetupResult) Failed() bool { return r.Failure != nil }

type setupTail struct {
	mu   sync.Mutex
	data []byte
}

func (w *setupTail) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(value)
	if len(value) >= worktreeSetupOutputLimit {
		w.data = append(w.data[:0], value[len(value)-worktreeSetupOutputLimit:]...)
	} else {
		w.data = append(w.data, value...)
		if len(w.data) > worktreeSetupOutputLimit {
			w.data = append(w.data[:0], w.data[len(w.data)-worktreeSetupOutputLimit:]...)
		}
	}
	return original, nil
}

// RunWorktreeSetup applies the resolved profile command in the job worktree.
func RunWorktreeSetup(worktreePath string, setup profile.WorktreeSetup) WorktreeSetupResult {
	if setup.Setup == nil {
		return WorktreeSetupResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(setup.SetupTimeoutSeconds)*time.Second)
	defer cancel()
	var shell string
	var args []string
	if runtime.GOOS == "windows" {
		shell = os.Getenv("COMSPEC")
		if shell == "" {
			shell = "cmd.exe"
		}
		args = []string{"/c", *setup.Setup}
	} else {
		shell = "/bin/sh"
		args = []string{"-c", *setup.Setup}
	}
	command := proc.Hide(exec.CommandContext(ctx, shell, args...))
	command.Dir = worktreePath
	proc.ConfigureProcessTreeKill(command)
	if runtime.GOOS == "windows" {
		field := reflect.ValueOf(command.SysProcAttr).Elem().FieldByName("CmdLine")
		if field.IsValid() && field.CanSet() {
			field.SetString(shell + " /c \"" + *setup.Setup + "\"")
		}
	}
	output := &setupTail{}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if err == nil {
		return WorktreeSetupResult{}
	}
	output.mu.Lock()
	text := bytesToPythonText(output.data)
	output.mu.Unlock()
	failure := &WorktreeSetupError{Output: text}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		failure.TimedOut = true
	} else {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code := exit.ExitCode()
			failure.ExitCode = &code
		} else {
			failure.Output = err.Error()
		}
	}
	return WorktreeSetupResult{Failure: failure}
}

func bytesToPythonText(value []byte) string {
	text := strings.ToValidUTF8(string(value), "\uFFFD")
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}
