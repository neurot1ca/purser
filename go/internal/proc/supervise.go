package proc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Launch describes a worker. File streams preserve independent output capture;
// control and owner liveness use private pipes never inherited by workers.
type Launch struct {
	Args          []string
	Env           []string
	Dir           string
	CreationFlags uint32
	HideWindow    bool
	Stdin         *os.File `json:"-"`
	Stdout        *os.File `json:"-"`
	Stderr        *os.File `json:"-"`
	Streams       [3]uintptr
}

type supervisorConfig struct {
	Job     string
	Streams [3]uintptr
}

func streamFiles(streams [3]uintptr) [3]*os.File {
	var files [3]*os.File
	for i, handle := range streams {
		if handle != 0 {
			files[i] = os.NewFile(handle, "worker-stream")
		}
	}
	return files
}

type SupervisionResult struct {
	// Confirmed proves the OS container is empty. Unix workers must retain
	// process-group membership; setsid/setpgid escape and guard killing are
	// outside that guarantee and cannot authorize recovery of escaped work.
	Confirmed bool
	Err       error
	RootExit  int
}

type supervisionEvent struct {
	Kind      string
	PID       int
	Exit      int
	Error     string
	Errno     uint64
	Confirmed bool
}

func eventError(e supervisionEvent) error {
	if e.Errno != 0 {
		return nativeError(e.Errno)
	}
	if e.Error != "" {
		return errors.New(e.Error)
	}
	return nil
}

func sendEvent(e supervisionEvent, err error) {
	if err != nil {
		e.Error = err.Error()
		e.Errno = errorNumber(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(e)
}

type Supervision struct {
	GuardPID int
	done     chan struct{}
	stop     sync.Once
	input    io.WriteCloser
	stopping chan struct{}
	result   SupervisionResult
}

func (s *Supervision) Done() <-chan struct{} { return s.done }

// Result is available after Done closes. Confirmed concerns teardown, not worker success.
func (s *Supervision) Result() SupervisionResult { <-s.done; return s.result }
func (s *Supervision) Stop()                     { s.stop.Do(func() { _ = s.input.Close(); close(s.stopping) }) }

// StartSupervised starts a same-binary guard. Prefix must dispatch SupervisorMain.
// The guard acknowledges containment before the owner admits the worker.
func StartSupervised(ctx context.Context, executable string, prefix []string, launch Launch) (*Supervision, error) {
	containment, name, err := newSupervisionContainment()
	if err != nil {
		return nil, err
	}
	defer containment.releaseBootstrap()
	command := Hide(exec.Command(executable, append(append([]string{}, prefix...), "guard", name)...))
	command.WaitDelay = time.Second
	configureGuard(command)
	containment.attach(command)
	cleanup, err := inheritWorkerStreams(command, &launch)
	if err != nil {
		containment.close()
		return nil, err
	}
	defer cleanup()
	config, err := json.Marshal(supervisorConfig{Job: name, Streams: launch.Streams})
	if err != nil {
		containment.close()
		return nil, err
	}
	command.Args[len(command.Args)-1] = string(config)
	input, err := command.StdinPipe()
	if err != nil {
		containment.close()
		return nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		containment.close()
		return nil, err
	}
	Hide(command)
	if err = command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		containment.close()
		return nil, err
	}
	s := &Supervision{GuardPID: command.Process.Pid, done: make(chan struct{}), stopping: make(chan struct{}), input: input}
	events := make(chan supervisionEvent, 4)
	go func() {
		defer close(events)
		dec := json.NewDecoder(output)
		for {
			var e supervisionEvent
			if err := dec.Decode(&e); err != nil {
				return
			}
			events <- e
		}
	}()
	go func() {
		defer close(s.done)
		defer containment.close()
		defer s.Stop()
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		var failure error
		stopping := s.stopping
		for {
			select {
			case e, ok := <-events:
				if !ok {
					if failure == nil {
						failure = errors.New("supervisor exited without teardown confirmation")
					}
					goto finished
				}
				if err := eventError(e); err != nil {
					failure = errors.Join(failure, err)
				}
				switch e.Kind {
				case "ready":
					if ctx.Err() != nil {
						failure = ctx.Err()
						s.Stop()
						continue
					}
					if err := json.NewEncoder(input).Encode(launch); err != nil {
						failure = err
						s.Stop()
					} else {
						timer.Stop()
					}
				case "root":
					s.result.RootExit = e.Exit
					if containment.owned() {
						confirmed, err := containment.stop()
						s.result.Confirmed = confirmed
						failure = errors.Join(failure, err)
						goto finished
					}
				case "stopped":
					s.result.Confirmed = e.Confirmed
					goto finished
				}
			case <-ctx.Done():
				failure = errors.Join(failure, ctx.Err())
				s.Stop()
				if containment.owned() {
					confirmed, err := containment.stop()
					s.result.Confirmed = confirmed
					failure = errors.Join(failure, err)
					goto finished
				}
				// Disable repeated cancellation while the Unix guard sweeps.
				ctx = context.Background()
				timer.Reset(4 * time.Second)
			case <-stopping:
				stopping = nil
				if containment.owned() {
					confirmed, err := containment.stop()
					s.result.Confirmed = confirmed
					failure = errors.Join(failure, err)
					goto finished
				}
				timer.Reset(4 * time.Second)
			case <-timer.C:
				failure = errors.Join(failure, errors.New("supervision teardown timed out"))
				s.Stop()
				if containment.owned() {
					_, err := containment.stop()
					failure = errors.Join(failure, err)
				}
				goto finished
			}
		}
	finished:
		// Windows termination kills the guard; Unix confirms before it exits.
		wait := make(chan error, 1)
		go func() { wait <- command.Wait() }()
		select {
		case err := <-wait:
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					s.result.Confirmed = false
					failure = errors.Join(failure, err)
				}
			}
		case <-time.After(time.Second):
			s.result.Confirmed = false
			failure = errors.Join(failure, errors.New("guard exit unconfirmed"))
		}
		s.result.Err = failure
	}()
	return s, nil
}

// SupervisorMain is a private same-binary entry point, never a worker API.
func SupervisorMain(args []string) int {
	prepareSupervisor()
	if len(args) != 2 {
		return 2
	}
	var config supervisorConfig
	if json.Unmarshal([]byte(args[1]), &config) != nil {
		return 2
	}
	if args[0] == "anchor" {
		return anchorMain()
	}
	if args[0] != "guard" {
		return 2
	}
	return guardMain(config)
}

func runWorker(launch Launch) {
	if len(launch.Args) == 0 {
		sendEvent(supervisionEvent{Kind: "root", Exit: -1}, exec.ErrNotFound)
		return
	}
	command := exec.Command(launch.Args[0], launch.Args[1:]...)
	command.Env, command.Dir = launch.Env, launch.Dir
	configureWorkerLaunch(command, launch)
	files := streamFiles(launch.Streams)
	command.Stdin, command.Stdout, command.Stderr = files[0], files[1], files[2]
	Hide(command)
	if err := command.Start(); err != nil {
		sendEvent(supervisionEvent{Kind: "root", Exit: -1}, err)
		return
	}
	err := command.Wait() // Files, not Go copy pipes: root exit is independent of EOF.
	exit := 0
	if err != nil {
		if command.ProcessState == nil {
			sendEvent(supervisionEvent{Kind: "root", PID: command.Process.Pid, Exit: -1}, err)
			return
		}
		exit = command.ProcessState.ExitCode()
	}
	sendEvent(supervisionEvent{Kind: "root", PID: command.Process.Pid, Exit: exit}, nil)
}

func supervisorPrefix() []string {
	for i, arg := range os.Args {
		if arg == "supervisor" {
			return append([]string{}, os.Args[1:i+1]...)
		}
	}
	return []string{"supervisor"}
}
