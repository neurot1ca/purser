//go:build linux || darwin

package proc

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

type supervisionContainment struct{}

var killSupervisedGroup = syscall.Kill
var observeSupervisedGroup = GroupStopped

const (
	ProcessAlive       = "ALIVE"
	ProcessDead        = "DEAD"
	ProcessUnconfirmed = "UNCONFIRMED"
)

func newSupervisionContainment() (supervisionContainment, string, error) {
	return supervisionContainment{}, "", nil
}
func (supervisionContainment) owned() bool         { return false }
func (supervisionContainment) close()              {}
func (supervisionContainment) releaseBootstrap()   {}
func (supervisionContainment) attach(*exec.Cmd)    {}
func (supervisionContainment) stop() (bool, error) { return false, nil }
func configureGuard(command *exec.Cmd)             { command.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func nativeError(n uint64) error                   { return syscall.Errno(n) }
func errorNumber(err error) uint64 {
	var n syscall.Errno
	if errors.As(err, &n) {
		return uint64(n)
	}
	return 0
}
func processStopped(pid int) bool { state, _ := ProcessIdentity(pid); return state == "DEAD" }

// A closed owner control pipe must not kill the guard before its sweep.
func prepareSupervisor() { signal.Ignore(syscall.SIGPIPE) }

func anchorMain() int {
	sendEvent(supervisionEvent{Kind: "ready"}, nil)
	var launch Launch
	if err := json.NewDecoder(os.Stdin).Decode(&launch); err != nil {
		return 1
	}
	runWorker(launch)
	// Keep the process-group identity pinned until the external guard kills it.
	for {
		time.Sleep(time.Hour)
	}
}

func inheritWorkerStreams(command *exec.Cmd, launch *Launch) (func(), error) {
	launch.Streams = [3]uintptr{}
	for i, file := range [3]*os.File{launch.Stdin, launch.Stdout, launch.Stderr} {
		if file != nil {
			launch.Streams[i] = uintptr(3 + len(command.ExtraFiles))
			command.ExtraFiles = append(command.ExtraFiles, file)
		}
	}
	return func() {}, nil
}

func guardMain(config supervisorConfig) int {
	command := Hide(exec.Command(os.Args[0], append(supervisorPrefix(), "anchor", "{}")...))
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	files := streamFiles(config.Streams)
	streams := Launch{Stdin: files[0], Stdout: files[1], Stderr: files[2]}
	_, err := inheritWorkerStreams(command, &streams)
	if err != nil {
		sendEvent(supervisionEvent{Kind: "stopped", Confirmed: true}, err)
		return 1
	}
	input, err := command.StdinPipe()
	if err != nil {
		sendEvent(supervisionEvent{Kind: "stopped", Confirmed: true}, err)
		return 1
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		sendEvent(supervisionEvent{Kind: "stopped", Confirmed: true}, err)
		return 1
	}
	Hide(command)
	if err = command.Start(); err != nil {
		sendEvent(supervisionEvent{Kind: "stopped", Confirmed: true}, err)
		return 1
	}
	// Never reap the anchor before the only group kill. Its unreleased identity
	// prevents a root-exit/PID-reuse race from targeting an unrelated group.
	events := make(chan supervisionEvent, 2)
	go func() {
		defer close(events)
		dec := json.NewDecoder(output)
		for {
			var e supervisionEvent
			if dec.Decode(&e) != nil {
				return
			}
			events <- e
		}
	}()
	owner := make(chan Launch, 1)
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		var launch Launch
		if json.NewDecoder(os.Stdin).Decode(&launch) != nil {
			return
		}
		owner <- launch
		_, _ = io.Copy(io.Discard, os.Stdin)
	}()
	var failure error
	ready := false
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				failure = errors.New("anchor observation failed")
				goto stop
			}
			if e.Kind == "ready" {
				ready = true
				sendEvent(e, nil)
			}
			if e.Kind == "root" {
				sendEvent(e, eventError(e))
				goto stop
			}
		case launch := <-owner:
			if !ready {
				failure = errors.New("worker admission before readiness")
				goto stop
			}
			select {
			case <-dead:
				goto stop
			default:
			}
			launch.Streams = streams.Streams
			if err := json.NewEncoder(input).Encode(launch); err != nil {
				failure = err
				goto stop
			}
			timer.Stop()
		case <-dead:
			goto stop
		case <-timer.C:
			failure = errors.New("anchor readiness timed out")
			goto stop
		}
	}
stop:
	confirmed, err := stopAnchoredGroup(command)
	sendEvent(supervisionEvent{Kind: "stopped", Confirmed: confirmed}, errors.Join(failure, err))
	if confirmed {
		return 0
	}
	return 1
}

func stopAnchoredGroup(command *exec.Cmd) (bool, error) {
	if err := killSupervisedGroup(-command.Process.Pid, syscall.SIGKILL); err != nil {
		return false, err
	}
	until := time.Now().Add(3 * time.Second)
	// Probe before releasing the pinned anchor; inert zombies do not execute.
	return confirmAnchoredGroup(command, until)
}

func confirmAnchoredGroup(command *exec.Cmd, until time.Time) (bool, error) {
	confirmed, err := observeAnchoredGroup(command.Process.Pid, until)
	// Reap only after the last group observation; never wait without a bound
	// for a process stuck in the kernel after accepted SIGKILL.
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case waitErr := <-wait:
		var exit *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exit) {
			return false, errors.Join(err, waitErr)
		}
	case <-time.After(time.Second):
		return false, errors.Join(err, errors.New("anchor exit unconfirmed"))
	}
	return confirmed, err
}

func observeAnchoredGroup(pid int, until time.Time) (bool, error) {
	for time.Now().Before(until) {
		stopped, err := observeSupervisedGroup(pid)
		if err != nil {
			return false, err
		}
		if stopped {
			return true, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false, errors.New("process group teardown confirmation timed out")
}
