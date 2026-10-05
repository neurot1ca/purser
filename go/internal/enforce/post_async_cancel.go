package enforce

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"purser-hook/internal/lease"
)

func cancelAsyncStart(root, call, jobID string) bool {
	if call == "" || jobID == "" {
		return false
	}
	sum := sha256.Sum256([]byte(call))
	if hex.EncodeToString(sum[:]) != jobID {
		return false
	}
	job, err := readAsyncObject(filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json"))
	if err != nil || job["jobId"] != jobID || job["call"] != call {
		return false
	}
	port, ok := asyncPort(job["signalPort"])
	if !ok {
		return false
	}
	token, ok := job["signalToken"].(string)
	if !ok || token == "" {
		return false
	}
	for _, char := range token {
		if char > 127 {
			return false
		}
	}
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return false
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = fmt.Fprintf(connection, "%s:%s:CANCEL", jobID, token); err != nil {
		return false
	}
	var response [1]byte
	_, err = connection.Read(response[:])
	return err == nil && response[0] == '1'
}

func asyncPort(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok {
		return 0, false
	}
	port := int64(number)
	if float64(port) != number {
		return 0, false
	}
	return int(port), port > 0 && port <= 65535
}

func asyncStartTerminalOrAbsent(root string, pending map[string]any) bool {
	call, ok := pending["async_call"].(string)
	if !ok || call == "" {
		return false
	}
	sum := sha256.Sum256([]byte(call))
	jobID := hex.EncodeToString(sum[:])
	path := filepath.Join(root, "codex_exec_mcp", "jobs", jobID+".json")
	job, err := readAsyncObject(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || job["jobId"] != jobID || job["call"] != call {
		return false
	}
	switch job["state"] {
	case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT":
		return true
	default:
		return false
	}
}

func cleanupTerminalAsyncStart(root string, pending, index map[string]any, now float64) (bool, error) {
	if !asyncStartTerminalOrAbsent(root, pending) {
		return false, nil
	}
	owner := stringValue(pending["lease_owner"])
	fromIndex := false
	if owner == "" {
		owner, fromIndex = stringValue(index["call_key"]), true
	}
	if owner == "" {
		return true, nil
	}
	project := stringValue(pending["project_key"])
	if project == "" {
		project = stringValue(index["project_key"])
	}
	if project == "" {
		return false, errors.New("async cleanup project key is missing")
	}
	result := lease.New(filepath.Join(root, "projects", project)).Release(owner, now)
	if result.Status == "OK" || (fromIndex && result.Code == "LEASE_OWNER_MISSING") {
		return true, nil
	}
	return false, errors.New(result.Code)
}
