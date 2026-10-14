package review

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

var defaultSupervisorPrefix = []string{"supervisor"}
var supervisorPrefix = append([]string(nil), defaultSupervisorPrefix...)

var errTeardownUnconfirmed = errors.New("reviewer process-tree teardown unconfirmed")

type supervisedExitError int

func (e supervisedExitError) Error() string {
	return "worker exited with status " + strconv.Itoa(int(e))
}
func (e supervisedExitError) ExitCode() int { return int(e) }

func isTestBinary(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(name, ".test") || strings.HasSuffix(name, ".test.exe")
}

func useSupervisor(executable string) bool {
	return !(isTestBinary(executable) && slicesEqual(supervisorPrefix, defaultSupervisorPrefix))
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func runDirect(ctx context.Context, args []string, cwd string, env []string, stdin io.Reader, stdout, stderr io.Writer, options ...ProcessOptions) (int, error) {
	command := proc.Hide(exec.CommandContext(ctx, args[0], args[1:]...))
	command.Dir, command.Env = cwd, env
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	if len(options) > 0 {
		configureNativeLaunch(command, options[0])
	}
	proc.ConfigureProcessTreeKill(command)
	err := command.Run()
	if command.ProcessState == nil {
		return -1, err
	}
	code := command.ProcessState.ExitCode()
	if err != nil {
		return code, err
	}
	return code, nil
}

func runSupervised(ctx context.Context, args []string, cwd string, env []string, stdin io.Reader, stdout, stderr io.Writer, options ...ProcessOptions) (int, error) {
	if len(args) == 0 {
		return -1, os.ErrInvalid
	}
	var input *os.File
	var inputWriter *os.File
	var err error
	if stdin != nil {
		input, inputWriter, err = os.Pipe()
		if err != nil {
			return -1, err
		}
	}
	makePipe := func() (*os.File, *os.File, error) { return os.Pipe() }
	outRead, outWrite, err := makePipe()
	if err != nil {
		if input != nil {
			_ = input.Close()
			_ = inputWriter.Close()
		}
		return -1, err
	}
	combined := stdout != nil && stderr != nil && stdout == stderr
	var errRead, errWrite *os.File
	if combined {
		errWrite = outWrite
	} else {
		errRead, errWrite, err = makePipe()
		if err != nil {
			_ = outRead.Close()
			_ = outWrite.Close()
			if input != nil {
				_ = input.Close()
				_ = inputWriter.Close()
			}
			return -1, err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		_ = outRead.Close()
		_ = outWrite.Close()
		if errRead != nil {
			_ = errRead.Close()
		}
		if errWrite != outWrite {
			_ = errWrite.Close()
		}
		if input != nil {
			_ = input.Close()
			_ = inputWriter.Close()
		}
		return -1, err
	}
	if !useSupervisor(executable) {
		_ = outRead.Close()
		_ = outWrite.Close()
		if errRead != nil {
			_ = errRead.Close()
		}
		if errWrite != outWrite {
			_ = errWrite.Close()
		}
		if input != nil {
			_ = input.Close()
			_ = inputWriter.Close()
		}
		return runDirect(ctx, args, cwd, env, stdin, stdout, stderr, options...)
	}
	launch := proc.Launch{Args: args, Dir: cwd, Env: env, Stdin: input, Stdout: outWrite, Stderr: errWrite}
	if len(options) > 0 {
		launch.CreationFlags, launch.HideWindow = options[0].CreationFlags, options[0].HideWindow
	}
	supervision, err := proc.StartSupervised(ctx, executable, supervisorPrefix, launch)
	_ = outWrite.Close()
	if errWrite != outWrite {
		_ = errWrite.Close()
	}
	if input != nil {
		_ = input.Close()
	}
	if err != nil {
		_ = outRead.Close()
		if errRead != nil {
			_ = errRead.Close()
		}
		if inputWriter != nil {
			_ = inputWriter.Close()
		}
		return -1, err
	}
	if inputWriter != nil {
		go func() { _, _ = io.Copy(inputWriter, stdin); _ = inputWriter.Close() }()
	}
	copyDone := make(chan struct{}, 2)
	go func() {
		if stdout != nil {
			_, _ = io.Copy(stdout, outRead)
		} else {
			_, _ = io.Copy(io.Discard, outRead)
		}
		_ = outRead.Close()
		copyDone <- struct{}{}
	}()
	if !combined {
		go func() {
			if stderr != nil {
				_, _ = io.Copy(stderr, errRead)
			} else {
				_, _ = io.Copy(io.Discard, errRead)
			}
			_ = errRead.Close()
			copyDone <- struct{}{}
		}()
	}
	result := supervision.Result()
	if !result.Confirmed {
		_ = outRead.Close()
		if errRead != nil {
			_ = errRead.Close()
		}
		return result.RootExit, errors.Join(result.Err, errTeardownUnconfirmed)
	}
	pending := 2
	if combined {
		pending = 1
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for pending > 0 {
		select {
		case <-copyDone:
			pending--
		case <-timer.C:
			_ = outRead.Close()
			if errRead != nil {
				_ = errRead.Close()
			}
			return result.RootExit, errors.Join(result.Err, errTeardownUnconfirmed, errors.New("worker output pipe remained open after tree teardown"))
		}
	}
	if result.Err != nil {
		if result.RootExit < 0 && (strings.Contains(strings.ToLower(result.Err.Error()), "executable file not found") || strings.Contains(strings.ToLower(result.Err.Error()), "cannot find the file")) {
			return result.RootExit, exec.ErrNotFound
		}
		return result.RootExit, result.Err
	}
	if result.RootExit != 0 {
		return result.RootExit, supervisedExitError(result.RootExit)
	}
	return result.RootExit, nil
}
