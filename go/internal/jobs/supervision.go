package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"purser-hook/internal/proc"
)

type treeReceipt struct {
	Confirmed bool   `json:"confirmed"`
	Error     string `json:"error,omitempty"`
}

var errTreeUnconfirmed = errors.New("Process termination unconfirmed")

func prepareTreeReceipt(dir, id, previous string) (string, error) {
	path := previous
	if path != "" {
		if stopped, err := treeStopped(dir, id, path); !stopped {
			return "", errors.Join(err, errTreeUnconfirmed)
		}
	} else {
		file, err := os.CreateTemp(dir, id+".tree-")
		if err != nil {
			return "", err
		}
		path = file.Name()
		if err := file.Close(); err != nil {
			return "", err
		}
	}
	return path, writeTreeReceipt(path, false, nil)
}

func writeTreeReceipt(path string, confirmed bool, err error) error {
	r := treeReceipt{Confirmed: confirmed}
	if err != nil {
		r.Error = err.Error()
	}
	data, marshalErr := json.Marshal(r)
	if marshalErr != nil {
		return marshalErr
	}
	return atomicWrite(path, data)
}

// treeStopped never substitutes a dead root or guard for container confirmation.
func treeStopped(dir, id, path string) (bool, error) {
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(dir) || !strings.HasPrefix(filepath.Base(path), id+".tree-") {
		return false, errors.New("invalid tree receipt path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("tree receipt is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var r treeReceipt
	if err = json.Unmarshal(data, &r); err != nil {
		return false, err
	}
	if r.Error != "" && !r.Confirmed {
		return false, errors.New(r.Error)
	}
	if !r.Confirmed {
		return false, errors.New("Process termination unconfirmed")
	}
	return true, nil
}

// DurableSupervisorMain records the Unix guard's sweep even when its host dies.
// Ordinary foundation/anchor invocations keep the M3 protocol unchanged.
func DurableSupervisorMain(args []string) int {
	if len(args) != 3 {
		return proc.SupervisorMain(args)
	}
	path := args[0]
	reader, writer, err := os.Pipe()
	if err != nil {
		return 1
	}
	output := os.Stdout
	os.Stdout = writer
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer reader.Close()
		dec := json.NewDecoder(reader)
		for {
			var event struct {
				Kind      string
				Confirmed bool
				Error     string
			}
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				return
			}
			if json.Unmarshal(raw, &event) == nil && event.Kind == "stopped" {
				var stopErr error
				if event.Error != "" {
					stopErr = errors.New(event.Error)
				}
				if err := writeTreeReceipt(path, event.Confirmed, stopErr); err != nil {
					// The owner must not accept a stop whose proof could not be saved.
					event.Confirmed = false
					event.Error = errors.Join(stopErr, err).Error()
					raw, _ = json.Marshal(event)
				}
			}
			_, _ = output.Write(append(raw, '\n'))
		}
	}()
	code := proc.SupervisorMain(args[1:])
	_ = writer.Close()
	<-done
	os.Stdout = output
	return code
}

// supervisedCommand adapts existing event/log writers to M3's file-only streams.
// Completion observes root exit before draining inherited descendant pipes.
type supervisedCommand struct {
	*proc.Supervision
	readers []*os.File
	copies  chan error
	count   int
	receipt string
}

func startSupervisedCommand(ctx context.Context, cmd *exec.Cmd, receipt string) (*supervisedCommand, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	s := &supervisedCommand{receipt: receipt, copies: make(chan error, 3)}
	launch := proc.Launch{Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir}
	var inherited []*os.File
	defer func() {
		for _, file := range inherited {
			_ = file.Close()
		}
	}()
	for i, stream := range []any{cmd.Stdin, cmd.Stdout, cmd.Stderr} {
		if stream == nil {
			continue
		}
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			for _, file := range s.readers {
				_ = file.Close()
			}
			return nil, pipeErr
		}
		s.count++
		if i == 0 {
			launch.Stdin = reader
			inherited = append(inherited, reader)
			s.readers = append(s.readers, writer)
			go func() { _, err := io.Copy(writer, stream.(io.Reader)); _ = writer.Close(); s.copies <- err }()
		} else {
			inherited = append(inherited, writer)
			s.readers = append(s.readers, reader)
			if i == 1 {
				launch.Stdout = writer
			} else {
				launch.Stderr = writer
			}
			go func() { _, err := io.Copy(stream.(io.Writer), reader); s.copies <- err }()
		}
	}
	prefix := []string{"supervisor"}
	if receipt != "" {
		prefix = append(prefix, receipt)
	}
	s.Supervision, err = proc.StartSupervised(ctx, executable, prefix, launch)
	if err != nil {
		for _, file := range s.readers {
			_ = file.Close()
		}
		return nil, err
	}
	return s, nil
}

func (s *supervisedCommand) wait() (proc.SupervisionResult, error) {
	r := s.Result()
	if s.receipt != "" {
		if err := writeTreeReceipt(s.receipt, r.Confirmed, r.Err); err != nil {
			r.Confirmed = false
			r.Err = errors.Join(r.Err, err)
		}
	}
	defer func() {
		for _, file := range s.readers {
			_ = file.Close()
		}
	}()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for i := 0; i < s.count; i++ {
		select {
		case err := <-s.copies:
			r.Err = errors.Join(r.Err, err)
		case <-timer.C:
			return r, errors.Join(r.Err, errors.New("supervised output drain unconfirmed"))
		}
	}
	if !r.Confirmed {
		return r, errors.Join(r.Err, errTreeUnconfirmed)
	}
	if r.Err != nil {
		return r, r.Err
	}
	if r.RootExit != 0 {
		return r, fmt.Errorf("worker exited with code %d", r.RootExit)
	}
	return r, nil
}

func recordedTreeStopped(dir string, job *Record) (bool, error) {
	var path string
	if !job.Get("treeReceipt", &path) {
		return false, errors.New("tree receipt missing")
	}
	var id string
	job.Get("jobId", &id)
	return treeStopped(dir, id, path)
}
