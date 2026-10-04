package enforce

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	maxTaskUsageLine  = 1_000_000
	maxTaskUsageBytes = 50_000_000
	maxTaskUsageRows  = 50_000
)

var safeTaskUsageID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

const (
	taskUsageLockTimeout = 500 * time.Millisecond
	taskUsageLockPoll    = 10 * time.Millisecond
)

func taskUsageLockPath(root, session string) string {
	return filepath.Join(root, "active_tasks", ".locks", session+".lock")
}

func taskUsageContentionPath(root, session string) string {
	return filepath.Join(root, "active_tasks", session+".contention")
}

// acquireTaskUsageLock mirrors session_lock: OS-owned locking is released if
// this process exits, and contention waits only until timeout.
func acquireTaskUsageLock(root, session string, timeout time.Duration) (*os.File, bool) {
	path := taskUsageLockPath(root, session)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, false
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false
	}
	if info, statErr := file.Stat(); statErr != nil {
		file.Close()
		return nil, false
	} else if info.Size() == 0 {
		// Python ignores a failed seed write and still tries to lock.
		_, _ = file.WriteAt([]byte{'0'}, 0)
	}
	deadline := time.Now().Add(timeout)
	for {
		if err = lockTaskUsageFile(file); err == nil {
			return file, true
		}
		if !time.Now().Before(deadline) {
			file.Close()
			return nil, false
		}
		time.Sleep(taskUsageLockPoll)
	}
}

func releaseTaskUsageLock(file *os.File) {
	if file != nil {
		_ = unlockTaskUsageFile(file)
		_ = file.Close()
	}
}

// markTaskUsageContention uses exclusive creation so the first task ID stays
// authoritative when multiple writers lose the session lock.
func markTaskUsageContention(root, session, taskID string) string {
	path := taskUsageContentionPath(root, session)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "FAILED"
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return "EXISTS"
	}
	if err != nil {
		return "FAILED"
	}
	defer file.Close()
	if taskID != "" && safeTaskUsageID.MatchString(taskID) {
		if _, err = file.WriteString(taskID); err != nil {
			return "FAILED"
		}
		if err = file.Sync(); err != nil {
			return "FAILED"
		}
	}
	return "CREATED"
}

func readTaskUsageContention(root, session string) (bool, string) {
	path := taskUsageContentionPath(root, session)
	if _, err := os.Stat(path); err != nil {
		return false, ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return true, ""
	}
	id := strings.TrimSpace(string(data))
	if !safeTaskUsageID.MatchString(id) {
		id = ""
	}
	return true, id
}

func removeTaskUsageContention(root, session string) bool {
	path := taskUsageContentionPath(root, session)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || info.IsDir() {
		return false
	}
	_ = os.Remove(path)
	_, err = os.Stat(path)
	return os.IsNotExist(err)
}

func activeTaskPath(root, session string) string {
	return filepath.Join(root, "active_tasks", session+".json")
}

func taskUsageFileIdentity(info os.FileInfo) (any, any, any) {
	if info == nil || info.Sys() == nil {
		return nil, nil, nil
	}
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	field := func(name string) (int64, bool) {
		f := v.FieldByName(name)
		if !f.IsValid() {
			return 0, false
		}
		switch f.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return f.Int(), true
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return int64(f.Uint()), true
		default:
			return 0, false
		}
	}
	dev, hasDev := field("Dev")
	ino, hasIno := field("Ino")
	var ctime any
	ct := v.FieldByName("Ctim")
	if !ct.IsValid() {
		ct = v.FieldByName("Ctimespec")
	}
	if ct.IsValid() {
		sec, nsec := int64(0), int64(0)
		if ct.Kind() == reflect.Struct {
			secField := ct.FieldByName("Sec")
			nsecField := ct.FieldByName("Nsec")
			if secField.IsValid() && nsecField.IsValid() {
				sec = secField.Int()
				nsec = nsecField.Int()
				ctime = sec*1_000_000_000 + nsec
			}
		}
	}
	var devValue, inoValue any
	if hasDev {
		devValue = dev
	}
	if hasIno {
		inoValue = ino
	}
	return devValue, inoValue, ctime
}

func writeActiveTaskJSON(path string, record map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err = temp.Write(data); err == nil {
		err = temp.Close()
	} else {
		_ = temp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func taskUsageUnknown(reason string) map[string]any {
	return map[string]any{"status": "DENIED", "reason": reason}
}

// startActiveTask mirrors start_active_task. The explicit clock keeps stale
// boundary decisions and created_at deterministic for parity checks.
func startActiveTask(root, session, taskID, projectKey, toolUseID, transcriptPath, projectsRoot string, now time.Time) map[string]any {
	if session == "" || taskID == "" || toolUseID == "" {
		return map[string]any{"status": "SKIPPED", "reason": "MISSING_IDENTIFIER"}
	}
	activePath := activeTaskPath(root, session)
	lock, acquired := acquireTaskUsageLock(root, session, taskUsageLockTimeout)
	if !acquired {
		markTaskUsageContention(root, session, taskID)
		return taskUsageUnknown("ACTIVE_TASK_STATE_CONTENTION")
	}
	defer releaseTaskUsageLock(lock)

	markerPresent, competingID := readTaskUsageContention(root, session)
	var existing map[string]any
	if data, err := os.ReadFile(activePath); err == nil {
		_ = json.Unmarshal(data, &existing)
	} else if !os.IsNotExist(err) {
		_ = os.Remove(activePath)
	}
	if markerPresent && existing != nil && existing["completed"] != true {
		affected, _ := existing["affected_task_ids"].([]any)
		affected = append([]any(nil), affected...)
		currentID := existing["task_id"]
		if currentID != nil && !containsTaskID(affected, currentID) {
			affected = append(affected, currentID)
		}
		if competingID != "" && !containsTaskID(affected, competingID) {
			affected = append(affected, competingID)
		}
		updated := cloneTaskRecord(existing)
		updated["invalidated"], updated["invalidated_reason"], updated["completed"] = true, "ACTIVE_TASK_STATE_CONTENTION", true
		updated["affected_task_ids"] = lastTaskIDs(affected, 16)
		if err := writeActiveTaskJSON(activePath, updated); err != nil {
			return taskUsageUnknown("ACTIVE_TASK_STATE_CONTENTION")
		}
		existing = updated
	}
	if markerPresent && !removeTaskUsageContention(root, session) {
		return taskUsageUnknown("ACTIVE_TASK_STATE_CONTENTION")
	}

	terminalIDs := []any{}
	if competingID != "" {
		terminalIDs = append(terminalIDs, competingID)
	}
	if existing != nil {
		stale := false
		if created, ok := existing["created_at"].(string); ok && created != "" {
			parsed, err := time.Parse(time.RFC3339Nano, strings.Replace(created, "Z", "+00:00", 1))
			stale = err != nil || now.Sub(parsed) > 24*time.Hour
		}
		if stale {
			_ = os.Remove(activePath)
			existing = nil
		} else if existing["completed"] == true {
			affected, ok := existing["affected_task_ids"].([]any)
			if !ok {
				affected = []any{existing["task_id"]}
			}
			priorTerminal, _ := existing["terminal_task_ids"].([]any)
			if taskID == existing["task_id"] || containsTaskID(affected, taskID) || containsTaskID(priorTerminal, taskID) {
				reason, ok := existing["invalidated_reason"].(string)
				if !ok || reason == "" {
					reason = "ALREADY_COMPLETED"
				}
				return taskUsageUnknown(reason)
			}
			for _, id := range affected {
				if id != nil && !containsTaskID(priorTerminal, id) {
					priorTerminal = append(priorTerminal, id)
				}
			}
			terminalIDs = lastTaskIDs(priorTerminal, 16)
			_ = os.Remove(activePath)
			existing = nil
		} else {
			if existing["task_id"] == taskID {
				return map[string]any{"status": "RETAINED", "active_task": existing}
			}
			updated := cloneTaskRecord(existing)
			updated["invalidated"], updated["invalidated_reason"], updated["completed"] = true, "OVERLAPPING_TASK_BOUNDARY", true
			updated["affected_task_ids"] = []any{existing["task_id"], taskID}
			if err := writeActiveTaskJSON(activePath, updated); err != nil {
				return taskUsageUnknown("OVERLAPPING_TASK_BOUNDARY")
			}
			return taskUsageUnknown("OVERLAPPING_TASK_BOUNDARY")
		}
	}
	if containsTaskID(terminalIDs, taskID) {
		return taskUsageUnknown("ACTIVE_TASK_STATE_CONTENTION")
	}

	var resolved string
	var identity *transcriptIdentity
	anchor, cursor, deferred := any(nil), int64(0), true
	if transcriptPath != "" {
		var reason string
		resolved, reason = validateTranscriptPath(transcriptPath, session, projectsRoot)
		if reason != "" {
			resolved = ""
		} else {
			identity, _ = snapshotTranscript(resolved)
			file, err := os.Open(resolved)
			if err != nil {
				return taskUsageUnknown("TRANSCRIPT_UNAVAILABLE")
			}
			var why string
			var found *int64
			found, cursor, deferred, why = scanTaskUsageAnchor(file, toolUseID)
			_ = file.Close()
			if why != "" {
				return taskUsageUnknown(why)
			}
			anchor = found
		}
	}
	var canonical any
	var dev, ino, ctime any
	initialSize := int64(0)
	if identity != nil {
		canonical, initialSize = identity.path, identity.size
		dev, ino, ctime = taskUsageFileIdentity(identity.info)
	} else if transcriptPath != "" {
		canonical = transcriptPath
	}
	record := map[string]any{
		"session_id": session, "task_id": taskID, "project_key": nullableString(projectKey), "tool_use_id": toolUseID,
		"canonical_transcript_path": canonical, "transcript_dev": dev, "transcript_ino": ino, "transcript_ctime_ns": ctime,
		"initial_size": initialSize, "anchor_byte_offset": anchor, "anchor_search_start_offset": cursor, "anchor_deferred": deferred,
		"created_at": now.UTC().Format("2006-01-02T15:04:05.000000Z"), "completed": false, "invalidated": false,
		"invalidated_reason": nil, "affected_task_ids": []any{taskID}, "terminal_task_ids": terminalIDs,
	}
	if err := writeActiveTaskJSON(activePath, record); err != nil {
		return taskUsageUnknown("ACTIVE_TASK_STATE_CONTENTION")
	}
	return map[string]any{"status": "STARTED", "active_task": record}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func cloneTaskRecord(record map[string]any) map[string]any {
	copy := make(map[string]any, len(record))
	for key, value := range record {
		copy[key] = value
	}
	return copy
}

func containsTaskID(values []any, id any) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}

func lastTaskIDs(values []any, limit int) []any {
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}

type transcriptIdentity struct {
	path string
	info os.FileInfo
	size int64
}

// validateTranscriptPath mirrors the Python oracle's path, session, and root checks.
func validateTranscriptPath(path, session, projectsRoot string) (string, string) {
	if path == "" || session == "" {
		return "", "TRANSCRIPT_UNAVAILABLE"
	}
	if !safeTaskUsageID.MatchString(session) {
		return "", "SESSION_MISMATCH"
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", "TRANSCRIPT_PATH_ESCAPE"
	}
	if strings.ToLower(filepath.Ext(path)) != ".jsonl" {
		return "", "WRONG_EXTENSION"
	}
	if strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) != session {
		return "", "SESSION_MISMATCH"
	}
	root, err := filepath.EvalSymlinks(projectsRoot)
	if err != nil {
		return "", "TRANSCRIPT_PATH_ESCAPE"
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "TRANSCRIPT_UNAVAILABLE"
		}
		return "", "TRANSCRIPT_PATH_ESCAPE"
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "TRANSCRIPT_PATH_ESCAPE"
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", "TRANSCRIPT_UNAVAILABLE"
	}
	return resolved, ""
}

func snapshotTranscript(path string) (*transcriptIdentity, string) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "TRANSCRIPT_UNAVAILABLE"
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "TRANSCRIPT_UNAVAILABLE"
	}
	canonical = filepath.Clean(canonical)
	if runtime.GOOS == "windows" {
		canonical = strings.ToLower(canonical)
	}
	return &transcriptIdentity{path: canonical, info: info, size: info.Size()}, ""
}

func checkTranscriptIdentity(path string, expected *transcriptIdentity, anchor, cursor *int64) string {
	current, reason := snapshotTranscript(path)
	if reason != "" {
		return reason
	}
	if current.path != expected.path || !os.SameFile(expected.info, current.info) {
		return "TRANSCRIPT_IDENTITY_MISMATCH"
	}
	if current.size < expected.size || (anchor != nil && *anchor > current.size) || (cursor != nil && *cursor > current.size) {
		return "TRANSCRIPT_ROTATED_OR_TRUNCATED"
	}
	return ""
}

func taskUsageLine(r *bufio.Reader) ([]byte, error) { return r.ReadBytes('\n') }

// scanTaskUsageAnchor seeks into a bounded tail and returns the found anchor,
// complete-line cursor, deferred flag, and Python-compatible rejection reason.
func scanTaskUsageAnchor(file *os.File, toolID string) (*int64, int64, bool, string) {
	info, err := file.Stat()
	if err != nil {
		return nil, 0, true, "TRANSCRIPT_UNAVAILABLE"
	}
	return scanTaskUsageTail(file, info.Size(), toolID)
}

func scanTaskUsageTail(file io.ReadSeeker, size int64, toolID string) (*int64, int64, bool, string) {
	start := size - maxTaskUsageBytes
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, 0, true, "TRANSCRIPT_UNAVAILABLE"
	}
	r := bufio.NewReader(file)
	if start > 0 {
		if _, err := taskUsageLine(r); err != nil && err != io.EOF {
			return nil, 0, true, "TRANSCRIPT_UNAVAILABLE"
		}
	}
	position, _ := file.Seek(0, io.SeekCurrent)
	cursor := position - int64(r.Buffered())
	anchorFound := false
	var anchor *int64
	count := 0
	for {
		lineStart := cursor
		line, readErr := taskUsageLine(r)
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		if len(line) > maxTaskUsageLine {
			return nil, cursor, true, "BOUNDS_EXCEEDED"
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		cursor += int64(len(line))
		count++
		if count > maxTaskUsageRows {
			return nil, cursor, true, "BOUNDS_EXCEEDED"
		}
		if !anchorFound {
			var row map[string]any
			if json.Unmarshal(line, &row) == nil && row["type"] == "assistant" {
				if msg, ok := row["message"].(map[string]any); ok {
					if content, ok := msg["content"].([]any); ok {
						for _, item := range content {
							block, ok := item.(map[string]any)
							if ok && block["type"] == "tool_use" && block["id"] == toolID {
								anchor, anchorFound = &lineStart, true
								break
							}
						}
					}
				}
			}
		}
		if readErr != nil && readErr != io.EOF {
			return nil, 0, true, "TRANSCRIPT_UNAVAILABLE"
		}
	}
	return anchor, cursor, !anchorFound, ""
}

func readTaskUsageRecords(file *os.File, offset int64, allowIncomplete bool) ([]map[string]any, string) {
	info, err := file.Stat()
	if err != nil {
		return nil, "TRANSCRIPT_UNAVAILABLE"
	}
	if offset > info.Size() {
		return nil, "TRANSCRIPT_ROTATED_OR_TRUNCATED"
	}
	if info.Size()-offset > maxTaskUsageBytes {
		return nil, "BOUNDS_EXCEEDED"
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return nil, "TRANSCRIPT_UNAVAILABLE"
	}
	r := bufio.NewReader(file)
	records := []map[string]any{}
	count, tail := 0, false
	for {
		line, readErr := taskUsageLine(r)
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		if len(line) > maxTaskUsageLine {
			return nil, "BOUNDS_EXCEEDED"
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			tail = true
			break
		}
		count++
		if count > maxTaskUsageRows {
			return nil, "BOUNDS_EXCEEDED"
		}
		var value any
		if json.Unmarshal(line, &value) != nil {
			return nil, "INVALID_USAGE_RECORD"
		}
		if row, ok := value.(map[string]any); ok && row["type"] == "assistant" {
			records = append(records, row)
		}
		if readErr != nil && readErr != io.EOF {
			return nil, "TRANSCRIPT_UNAVAILABLE"
		}
	}
	if tail && !allowIncomplete {
		return nil, "TRANSCRIPT_UNRESOLVED_TAIL"
	}
	return records, ""
}
