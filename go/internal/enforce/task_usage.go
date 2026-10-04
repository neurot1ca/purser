package enforce

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
