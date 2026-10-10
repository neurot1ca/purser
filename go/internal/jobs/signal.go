package jobs

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const signalMessageLimit = 256
const signalCompleteReadBound = 50 * time.Millisecond

var signalWriteTimeout = 5 * time.Second

// SignalHost serves the loopback protocol used by purser_wait.py and the
// Python job host. Close it when the host exits. Close terminal only after the
// terminal job record (and any result output) has been durably written.
type SignalHost struct {
	listener net.Listener
	jobID    string
	token    string
	terminal <-chan struct{}
	cancel   func() bool
	done     chan struct{}
	close    sync.Once
}

func StartSignalHost(jobID, token string, terminal <-chan struct{}, cancel func() bool) (*SignalHost, error) {
	if !validSignalField(jobID) || !validSignalField(token) || terminal == nil {
		return nil, errors.New("invalid signal host configuration")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	h := &SignalHost{listener: listener, jobID: jobID, token: token,
		terminal: terminal, cancel: cancel, done: make(chan struct{})}
	go h.serve()
	return h, nil
}

func (h *SignalHost) Addr() net.Addr { return h.listener.Addr() }

func (h *SignalHost) Close() error {
	var err error
	h.close.Do(func() {
		close(h.done)
		err = h.listener.Close()
	})
	return err
}

func (h *SignalHost) serve() {
	for {
		connection, err := h.listener.Accept()
		if err != nil {
			return
		}
		go h.handle(connection)
	}
}

func (h *SignalHost) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	message := make([]byte, 0, signalMessageLimit+1)
	var parts []string
	bounded := false
	for {
		if len(message) > signalMessageLimit+1 {
			return
		}
		chunk := make([]byte, signalMessageLimit+1-len(message))
		n, err := connection.Read(chunk)
		if n > 0 {
			message = append(message, chunk[:n]...)
		}
		if len(message) > signalMessageLimit+1 || (len(message) > signalMessageLimit && message[len(message)-1] != '\n') {
			return
		}
		parts = nil
		candidate := message
		newline := len(candidate) > 0 && candidate[len(candidate)-1] == '\n'
		if newline {
			candidate = candidate[:len(candidate)-1]
		}
		parsed, valid := signalMessageParts(candidate)
		matches := valid && len(parsed) >= 2 && parsed[0] == h.jobID && parsed[1] == h.token
		if matches {
			parts = parsed
			if newline || err == io.EOF {
				break
			}
			if !bounded {
				_ = connection.SetReadDeadline(time.Now().Add(signalCompleteReadBound))
				bounded = true
			}
		} else if valid {
			if !signalMessageHostPrefix(candidate, h.jobID, h.token) {
				return
			}
		} else if newline || err == io.EOF {
			return
		}
		if err != nil {
			if parts != nil && bounded {
				break
			}
			return
		}
	}
	_ = connection.SetReadDeadline(time.Time{})
	switch {
	case len(parts) == 2:
		select {
		case <-h.terminal:
			_ = connection.SetWriteDeadline(time.Now().Add(signalWriteTimeout))
			_, _ = connection.Write([]byte("1"))
		case <-h.done:
		}
	case len(parts) == 3 && parts[2] == "PING":
		_ = connection.SetWriteDeadline(time.Now().Add(signalWriteTimeout))
		_, _ = connection.Write([]byte("1"))
	case len(parts) == 3 && parts[2] == "CANCEL" && h.cancel != nil:
		cancelled := h.cancel()
		_ = connection.SetWriteDeadline(time.Now().Add(signalWriteTimeout))
		if cancelled {
			_, _ = connection.Write([]byte("1"))
		} else {
			_, _ = connection.Write([]byte("0"))
		}
	}
}

func signalMessageParts(message []byte) ([]string, bool) {
	if len(message) == 0 || len(message) > signalMessageLimit {
		return nil, false
	}
	for i := range message {
		if message[i] > 0x7f {
			return nil, false
		}
	}
	parts := strings.Split(string(message), ":")
	if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[2] != "PING" && parts[2] != "CANCEL") {
		return nil, false
	}
	return parts, true
}

func signalMessageHostPrefix(message []byte, jobID, token string) bool {
	prefix := string(message)
	for _, suffix := range []string{"", ":PING", ":CANCEL"} {
		if strings.HasPrefix(jobID+":"+token+suffix, prefix) {
			return true
		}
	}
	return false
}

func SignalPing(address, jobID, token string, timeout time.Duration) (bool, error) {
	return signalRequest(address, jobID, token, "PING", timeout)
}

func SignalCancel(address, jobID, token string, timeout time.Duration) (bool, error) {
	return signalRequest(address, jobID, token, "CANCEL", timeout)
}

// SignalWait returns only after the host has acknowledged its durable terminal
// transition. timeout bounds both connection establishment and the wait.
func SignalWait(address, jobID, token string, timeout time.Duration) (bool, error) {
	return signalRequest(address, jobID, token, "", timeout)
}

func signalRequest(address, jobID, token, command string, timeout time.Duration) (bool, error) {
	if !validSignalField(jobID) || !validSignalField(token) || timeout <= 0 {
		return false, errors.New("invalid signal request")
	}
	message := jobID + ":" + token
	if command != "" {
		message += ":" + command
	}
	if len(message) > signalMessageLimit {
		return false, errors.New("signal request exceeds limit")
	}
	connection, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return false, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(timeout))
	if _, err = connection.Write([]byte(message)); err != nil {
		return false, err
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		if err = tcp.CloseWrite(); err != nil {
			return false, err
		}
	}
	var acknowledgement [1]byte
	if _, err = io.ReadFull(connection, acknowledgement[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return acknowledgement[0] == '1', nil
}

func validSignalField(value string) bool {
	if value == "" || strings.Contains(value, ":") {
		return false
	}
	for i := range value {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
