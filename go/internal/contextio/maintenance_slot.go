package contextio

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maintenanceJobTimeout      = 45 * time.Minute
	maintenanceBackgroundLimit = maintenanceQuietPeriod + maintenanceJobTimeout
)

var maintenanceTerminalJobs = map[string]bool{
	"SUCCEEDED": true, "FAILED": true, "CANCELLED": true,
	"TIMED_OUT": true, "UNKNOWN_OR_IN_PROGRESS": true,
}

// AcquireMaintenanceSlot owns the global maintenance slot until release is called.
func AcquireMaintenanceSlot(state string) (func(), error) {
	path := filepath.Join(state, "maintenance", "active.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := createMaintenanceSlot(path); err != nil {
		if !errors.Is(err, os.ErrExist) || !maintenanceGlobalSlotStale(path, time.Now()) {
			return nil, err
		}
		stalePath, err := maintenanceTemporaryPath(path, ".stale")
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, stalePath); err == nil {
			_ = os.Remove(stalePath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := createMaintenanceSlot(path); err != nil {
			return nil, err
		}
	}
	return func() { _ = os.Remove(path) }, nil
}

func createMaintenanceSlot(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	data := []byte(fmt.Sprintf(`{"pid": %d, "started_at": %s}`, os.Getpid(), strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'g', -1, 64)))
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func maintenanceGlobalSlotStale(path string, now time.Time) bool {
	value, ok := readMaintenanceObject(path)
	if !ok {
		return false
	}
	if started, ok := maintenancePythonNumber(value["started_at"]); ok && float64(now.UnixNano())/1e9-started > maintenanceBackgroundLimit.Seconds() {
		return true
	}
	pid, ok := value["pid"].(json.Number)
	if !ok || !jsonInteger(pid) {
		return false
	}
	var processID int
	if _, err := fmt.Sscan(pid.String(), &processID); err != nil || processID <= 0 {
		return false
	}
	return !maintenancePIDAlive(processID)
}

func maintenancePythonNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	case bool:
		if number {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// CreateMaintenanceActive writes a marker with Python's exclusive-create semantics.
func CreateMaintenanceActive(path string, active map[string]any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	data, writeErr := maintenanceActiveJSON(active)
	if writeErr == nil {
		_, writeErr = file.Write(data)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return writeErr
	}
	return closeErr
}

// CreateOrRecoverMaintenanceActive applies run_report's stale-marker recovery.
func CreateOrRecoverMaintenanceActive(activePath, state string, active map[string]any, now time.Time) error {
	if err := CreateMaintenanceActive(activePath, active); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrExist) || !MaintenanceActiveStale(activePath, state, now) {
		return err
	}
	stalePath, err := maintenanceTemporaryPath(activePath, ".stale")
	if err != nil {
		return err
	}
	if err := os.Rename(activePath, stalePath); err == nil {
		_ = os.Remove(stalePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return CreateMaintenanceActive(activePath, active)
}

func maintenanceActiveJSON(active map[string]any) ([]byte, error) {
	keys := []string{"project_root", "started_at", "state", "job_id"}
	ordered := make([]string, 0, len(active))
	for _, key := range keys {
		if _, ok := active[key]; ok {
			ordered = append(ordered, key)
		}
	}
	for key := range active {
		known := false
		for _, candidate := range keys {
			if key == candidate {
				known = true
				break
			}
		}
		if !known {
			ordered = append(ordered, key)
		}
	}
	parts := make([]string, 0, len(ordered))
	for _, key := range ordered {
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(active[key])
		if err != nil {
			return nil, err
		}
		parts = append(parts, string(name)+": "+string(value))
	}
	return []byte("{" + strings.Join(parts, ", ") + "}"), nil
}

// ReadMaintenanceActive returns a valid JSON object; malformed evidence is unknown.
func ReadMaintenanceActive(path string) (map[string]any, bool) { return readMaintenanceObject(path) }

func readMaintenanceObject(path string) (map[string]any, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil {
		return nil, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return value, true
}

// MaintenanceActiveStale mirrors _active_is_stale, including unknown evidence handling.
func MaintenanceActiveStale(activePath, state string, now time.Time) bool {
	active, ok := ReadMaintenanceActive(activePath)
	if !ok {
		return false
	}
	if rawID, exists := active["job_id"]; exists {
		jobID, ok := rawID.(string)
		if !ok || jobID == "" || strings.Trim(jobID, "0123456789abcdef") != "" {
			return true
		}
		job, valid := readSmallObject(filepath.Join(state, "codex_exec_mcp", "jobs", jobID+".json"))
		if !valid || job["jobId"] != jobID {
			return true
		}
		jobState, _ := job["state"].(string)
		return maintenanceTerminalJobs[jobState]
	}
	startedText, ok := active["started_at"].(string)
	if !ok {
		return false
	}
	started, err := time.Parse("20060102T150405.000000Z", startedText)
	return err == nil && now.Sub(started) >= maintenanceJobTimeout
}

// ClearMaintenanceActive removes a marker, matching run_report cleanup.
func ClearMaintenanceActive(path string) error { return os.Remove(path) }

func maintenanceTemporaryPath(path, suffix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "."+hex.EncodeToString(random[:])+suffix), nil
}
