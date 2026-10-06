package contextio

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const maxJournal = 64 << 10
const maxBytesPerRead = 256 << 10
const maxBytesPerTurn = 2 << 20
const maxLinesPerRead = 501
const maxLinesPerTurn = 8 * maxLinesPerRead
const maxStatsPerTurn = 160
const chunkBytes = 64 << 10

var protected = []string{".orchestrator/**", "AGENTS.md", "CLAUDE.md", ".claude/settings.json", ".claude/settings.local.json", ".git", ".git/**", ".env", ".env.*", "*.pem", "*.key", "credentials.*", "secrets.*"}

func sessionKey(v any) string {
	s, ok := v.(string)
	if !ok || s == "" || utf8.RuneCountInString(s) > 512 {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func repositoryProjectKey(repository string) string {
	if runtime.GOOS == "windows" {
		repository = strings.ToLower(repository)
	}
	h := sha256.Sum256([]byte(repository))
	return hex.EncodeToString(h[:])[:16]
}

func stateRoot() string {
	if p := os.Getenv("PURSER_STATE_DIR"); p != "" {
		return resolve(expand(p))
	}
	if p := os.Getenv("CLAUDE_CONFIG_DIR"); p != "" {
		return resolve(expand(filepath.Join(p, "purser")))
	}
	h, _ := os.UserHomeDir()
	return resolve(filepath.Join(h, ".claude", "purser"))
}
func resolve(p string) string {
	absolute, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	current := absolute
	suffix := []string{}
	for {
		real, e := filepath.EvalSymlinks(current)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return filepath.Clean(real)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absolute)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
func expand(p string) string {
	if strings.HasPrefix(p, "~") {
		end := strings.IndexAny(p, `/\`)
		if end < 0 {
			end = len(p)
		}
		name := p[1:end]
		home := ""
		if name == "" {
			home, _ = os.UserHomeDir()
		} else if runtime.GOOS == "windows" {
			if current, err := os.UserHomeDir(); err == nil {
				home = filepath.Join(filepath.Dir(current), name)
			}
		} else if account, err := user.Lookup(name); err == nil {
			home = account.HomeDir
		}
		if home != "" {
			p = filepath.Join(home, p[end:])
		}
	}
	a, e := filepath.Abs(p)
	if e == nil {
		return filepath.Clean(a)
	}
	return p
}

func projectRoot(event map[string]any) string {
	candidate := ""
	if ti, ok := event["tool_input"].(map[string]any); ok {
		candidate, _ = ti["cwd"].(string)
	}
	if strings.TrimSpace(candidate) == "" {
		candidate, _ = event["cwd"].(string)
	}
	if candidate == "" {
		return ""
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return ""
	}
	_, gitDirSet := os.LookupEnv("GIT_DIR")
	_, workTreeSet := os.LookupEnv("GIT_WORK_TREE")
	if !gitDirSet && !workTreeSet {
		if start, err := filepath.EvalSymlinks(abs); err == nil {
			if info, err := os.Stat(start); err == nil && info.IsDir() {
				for dir := start; ; dir = filepath.Dir(dir) {
					if marker, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (marker.IsDir() || marker.Mode().IsRegular()) {
						return dir
					}
					if parent := filepath.Dir(dir); parent == dir {
						break
					}
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = abs
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return resolve(strings.TrimSpace(string(out)))
}

// ProjectRoot resolves the repository associated with a hook event.
func ProjectRoot(event map[string]any) string { return projectRoot(event) }

func Read(event map[string]any, processStart time.Time) error {
	started := time.Now()
	key := sessionKey(event["session_id"])
	if key == "" {
		return nil
	}
	base := filepath.Join(stateRoot(), "context_io")
	journal := filepath.Join(base, "turns", key+".json")
	lockPath := filepath.Join(base, "locks", key+".lock")
	contention := filepath.Join(base, "contention", key)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		_, _ = f.WriteAt([]byte("0"), 0)
	}
	deadline := time.Now().Add(50 * time.Millisecond)
	acquired := false
	for time.Now().Before(deadline) {
		if tryLock(f) == nil {
			acquired = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !acquired {
		markContention(contention)
		return nil
	}
	defer unlock(f)
	turn, reason := loadTurn(journal)
	if turn == nil {
		turn = newTurn(reason)
		if reason == "" {
			turn["unknown_reason"] = "MISSING_USER_PROMPT"
		}
	}
	consumeContention(turn, contention)
	obs := observe(turn, event, projectRoot(event))
	addObservation(turn, obs)
	recordLatency(turn, time.Since(started).Microseconds(), time.Since(processStart).Microseconds())
	if err := writeTurn(journal, turn); err != nil {
		var sizeErr journalLimitError
		if errors.As(err, &sizeErr) {
			if len(turn["observations"].([]any)) > 64 {
				turn["observations"] = turn["observations"].([]any)[:64]
			}
			setReason(turn, "JOURNAL_LIMIT")
			if retryErr := writeTurn(journal, turn); isOSError(retryErr) {
				markContention(contention)
			}
		} else if isOSError(err) {
			markContention(contention)
		}
	}
	return nil
}

func newTurn(reason string) map[string]any {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return map[string]any{"schema_version": 1, "turn_nonce": hex.EncodeToString(b), "opened_at": float64(time.Now().UnixNano()) / 1e9, "observations": []any{}, "read_count": 0, "full_read_count": 0, "targeted_read_count": 0, "unknown_read_count": 0, "stat_operations": 0, "bytes_scanned": 0, "lines_scanned": 0, "task_association_known": false, "task_id": nil, "project_key": nil, "codex_boundary_index": nil, "unknown_reason": nullable(reason), "latency": []any{}}
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func loadTurn(path string) (map[string]any, string) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ""
	}
	if err != nil || len(b) > maxJournal {
		return nil, "CORRUPT_STATE"
	}
	var t map[string]any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if decoder.Decode(&t) != nil || !validTurn(t) {
		return nil, "CORRUPT_STATE"
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, "CORRUPT_STATE"
	}
	if time.Since(time.Unix(0, int64(num(t["opened_at"])*1e9))) > 6*time.Hour {
		return nil, "STALE_STATE"
	}
	return t, ""
}
func validTurn(t map[string]any) bool {
	version, ok := t["schema_version"].(json.Number)
	if !ok || num(version) != 1 || len(str(t["turn_nonce"])) != 32 {
		return false
	}
	if _, ok := t["opened_at"].(json.Number); !ok {
		return false
	}
	obs, ok := t["observations"].([]any)
	if !ok || len(obs) > 128 {
		return false
	}
	if _, ok := t["task_association_known"].(bool); !ok {
		return false
	}
	if _, ok := t["latency"].([]any); !ok {
		return false
	}
	for _, key := range []string{"read_count", "full_read_count", "targeted_read_count", "unknown_read_count", "stat_operations", "bytes_scanned", "lines_scanned"} {
		n, ok := t[key].(json.Number)
		if !ok {
			return false
		}
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil || i < 0 {
			return false
		}
	}
	if v := t["final_event"]; v != nil {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	return true
}
func str(v any) string { s, _ := v.(string); return s }
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := strconv.ParseFloat(string(n), 64)
		return f
	case int:
		return float64(n)
	}
	return 0
}
func writeTurn(path string, t map[string]any) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if len(b) > maxJournal {
		return journalLimitError{}
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return replaceWithRetry(name, path)
}
func replaceWithRetry(source, destination string) error {
	deadline := time.Now().Add(30 * time.Millisecond)
	for attempt := 0; attempt < 4; attempt++ {
		err := os.Rename(source, destination)
		if err == nil {
			return nil
		}
		if !isPermissionError(err) || attempt == 3 {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		delay := time.Duration(5*(1<<attempt)) * time.Millisecond
		if delay > remaining {
			delay = remaining
		}
		time.Sleep(delay)
	}
	return nil
}
func isPermissionError(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	return runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32))
}
func isOSError(err error) bool {
	var pathErr *os.PathError
	var linkErr *os.LinkError
	return errors.As(err, &pathErr) || errors.As(err, &linkErr)
}
func markContention(path string) {
	if os.MkdirAll(filepath.Dir(path), 0700) == nil {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			f.Close()
		}
	}
}
func consumeContention(t map[string]any, path string) {
	if _, err := os.Stat(path); err == nil {
		setReason(t, "STATE_CONTENTION")
		_ = os.Remove(path)
	}
}
func setReason(t map[string]any, reason string) {
	if reason != "" && t["unknown_reason"] == nil {
		t["unknown_reason"] = reason
	}
}
func recordLatency(t map[string]any, internal, wall int64) {
	a, ok := t["latency"].([]any)
	if ok && len(a) < 130 {
		a = append(a, []any{internal, wall})
		t["latency"] = a
	}
}

type journalLimitError struct{}

func (journalLimitError) Error() string { return "journal bound exceeded" }

func observe(t, event map[string]any, root string) map[string]any {
	ti, ok := event["tool_input"].(map[string]any)
	mode := "UNKNOWN"
	if ok {
		mode = "FULL"
		if _, a := ti["offset"]; a {
			mode = "TARGETED"
		}
		if _, a := ti["limit"]; a {
			mode = "TARGETED"
		}
	}
	reason := "INVALID_INPUT"
	raw, valid := ti["file_path"].(string)
	if ok && valid && raw != "" && utf8.RuneCountInString(raw) <= 4096 && !strings.ContainsRune(raw, 0) && root != "" {
		absroot := resolve(root)
		p := raw
		if !filepath.IsAbs(p) {
			p = filepath.Join(absroot, p)
		}
		if absroot != "" {
			p = resolve(p)
			rel, err := filepath.Rel(absroot, p)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				if isProtected(filepath.ToSlash(rel), profileProtectedPaths(absroot)) {
					reason = "PROTECTED_PATH"
				} else {
					if num(t["stat_operations"]) >= maxStatsPerTurn {
						return unknownObservation(mode, "RESOURCE_LIMIT")
					}
					t["stat_operations"] = num(t["stat_operations"]) + 1
					if info, statErr := os.Stat(p); statErr != nil {
						if errors.Is(statErr, os.ErrNotExist) {
							reason = "MISSING_FILE"
						} else {
							reason = "STAT_UNAVAILABLE"
						}
					} else if !info.Mode().IsRegular() {
						reason = "DIRECTORY"
					} else {
						return scanRead(t, p, info, mode, ti)
					}
				}
			} else {
				reason = "OUTSIDE_ROOT"
			}
		} else {
			reason = "OUTSIDE_ROOT"
		}
	}
	return unknownObservation(mode, reason)
}
func unknownObservation(mode, reason string) map[string]any {
	return map[string]any{"mode": mode, "bytes": nil, "line_kind": "UNKNOWN", "line_value": nil, "oversized": false, "binary_suspected": false, "thresholds": map[string]any{"200": nil, "350": nil, "500": nil}, "identity": nil, "repeat": nil, "reason": nullable(reason)}
}

func scanRead(t map[string]any, path string, info os.FileInfo, mode string, ti map[string]any) map[string]any {
	o := map[string]any{"mode": mode, "bytes": nil, "line_kind": "UNKNOWN", "line_value": nil, "oversized": false, "binary_suspected": false, "thresholds": map[string]any{"200": nil, "350": nil, "500": nil}, "identity": fileIdentity(path, info), "repeat": nil, "reason": "BOUNDED_SCAN"}
	if mode == "FULL" {
		o["bytes"] = max(int64(0), info.Size())
		o["oversized"] = info.Size() > maxBytesPerRead
	}
	for _, old := range t["observations"].([]any) {
		if m, ok := old.(map[string]any); ok && o["identity"] != nil && reflect.DeepEqual(m["identity"], o["identity"]) {
			o["repeat"] = true
			break
		}
	}
	if o["repeat"] == nil {
		if o["identity"] == nil {
			o["repeat"] = nil
		} else {
			o["repeat"] = false
		}
	}
	byteBudget := min(maxBytesPerRead, max(0, maxBytesPerTurn-int(num(t["bytes_scanned"]))))
	lineBudget := min(maxLinesPerRead, max(0, maxLinesPerTurn-int(num(t["lines_scanned"]))))
	if byteBudget <= 0 || lineBudget <= 0 {
		o["reason"] = "RESOURCE_LIMIT"
		return o
	}
	offset, limit := 1, 0
	if mode == "TARGETED" {
		if v, exists := ti["offset"]; exists {
			var ok bool
			offset, ok = positiveInt(v)
			if !ok {
				o["reason"] = "INVALID_INPUT"
				return o
			}
		}
		if v, exists := ti["limit"]; exists && v != nil {
			var ok bool
			limit, ok = positiveInt(v)
			if !ok {
				o["reason"] = "INVALID_INPUT"
				return o
			}
		}
	}
	var result scanResult
	var err error
	if mode == "TARGETED" {
		result, err = scanTargeted(path, offset, limit, byteBudget, lineBudget)
	} else {
		result, err = scanFull(path, byteBudget, lineBudget)
	}
	if err != nil {
		o["reason"] = "STAT_UNAVAILABLE"
		return o
	}
	t["bytes_scanned"] = num(t["bytes_scanned"]) + float64(result.bytes)
	t["lines_scanned"] = num(t["lines_scanned"]) + float64(result.lines)
	if mode == "TARGETED" {
		if result.binary {
			o["bytes"] = nil
		} else {
			o["bytes"] = result.selectedBytes
		}
	}
	o["line_kind"] = result.kind
	o["line_value"] = result.value
	o["binary_suspected"] = result.binary
	if result.binary {
		o["reason"] = "BINARY_SUSPECTED"
	} else if result.kind == "EXACT" {
		o["reason"] = nil
	}
	thresholds := o["thresholds"].(map[string]any)
	for _, threshold := range []int{200, 350, 500} {
		if result.kind == "EXACT" {
			thresholds[strconv.Itoa(threshold)] = result.value.(int) > threshold
		} else if result.kind == "LOWER_BOUND" && result.value.(int) > threshold {
			thresholds[strconv.Itoa(threshold)] = true
		}
	}
	return o
}
func positiveInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		if i, yes := v.(int); yes {
			if i >= 1 {
				return i, true
			}
			return 0, false
		}
		return 0, false
	}
	i, err := strconv.Atoi(string(n))
	return i, err == nil && i >= 1
}

type scanResult struct {
	kind          string
	value         any
	binary        bool
	bytes         int
	lines         int
	selectedBytes int
}

func scanFull(path string, byteBudget, lineBudget int) (scanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return scanResult{}, err
	}
	defer f.Close()
	var r scanResult
	r.kind = "LOWER_BOUND"
	eof := false
	var last byte
	buf := make([]byte, chunkBytes)
	for r.bytes < byteBudget && r.lines < lineBudget {
		request := min(chunkBytes, byteBudget-r.bytes)
		n, e := f.Read(buf[:request])
		if n == 0 {
			if e == io.EOF {
				eof = true
			}
			break
		}
		chunk := buf[:n]
		r.bytes += n
		if bytes.IndexByte(chunk, 0) >= 0 {
			r.binary = true
			r.kind = "UNKNOWN"
			r.value = nil
			return r, nil
		}
		newlines := bytes.Count(chunk, []byte{'\n'})
		allowed := lineBudget - r.lines
		if newlines > allowed {
			r.lines = lineBudget
			last = '\n'
			break
		}
		r.lines += newlines
		last = chunk[n-1]
		if n < request {
			eof = true
			break
		}
	}
	if !eof && r.bytes < byteBudget && r.lines < lineBudget {
		var one [1]byte
		n, e := f.Read(one[:])
		eof = n == 0 && e == io.EOF
	}
	value := r.lines
	if eof && r.bytes > 0 && last != '\n' {
		value++
	}
	if eof {
		r.kind = "EXACT"
	} else if value < r.lines {
		value = r.lines
	}
	r.value = value
	r.lines = value
	return r, nil
}
func scanTargeted(path string, offset, limit, byteBudget, lineBudget int) (scanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return scanResult{}, err
	}
	defer f.Close()
	var r scanResult
	r.kind = "UNKNOWN"
	current, selectedLines := 1, 0
	var last byte
	eof, complete := false, false
	buf := make([]byte, chunkBytes)
	for r.bytes < byteBudget && r.lines < lineBudget {
		request := min(chunkBytes, byteBudget-r.bytes)
		n, e := f.Read(buf[:request])
		if n == 0 {
			if e == io.EOF {
				eof = true
			}
			break
		}
		chunk := buf[:n]
		r.bytes += n
		if bytes.IndexByte(chunk, 0) >= 0 {
			r.binary = true
			return r, nil
		}
		last = chunk[n-1]
		for _, b := range chunk {
			selected := current >= offset && (limit == 0 || selectedLines < limit)
			if selected {
				r.selectedBytes++
			}
			if b != '\n' {
				continue
			}
			r.lines++
			if selected {
				selectedLines++
			}
			current++
			if limit > 0 && selectedLines >= limit {
				complete = true
				break
			}
			if r.lines >= lineBudget {
				break
			}
		}
		if complete || r.lines >= lineBudget {
			break
		}
		if n < request {
			eof = true
			break
		}
	}
	if eof && r.bytes > 0 && last != '\n' {
		r.lines++
		if current >= offset {
			selectedLines++
		}
	}
	if complete || eof {
		r.kind = "EXACT"
		r.value = selectedLines
	} else {
		r.value = nil
		r.selectedBytes = 0
	}
	return r, nil
}
func isProtected(p string, patterns []string) bool {
	insensitive := os.PathSeparator == '\\'
	if insensitive {
		p = strings.ToLower(p)
	}
	for _, pat := range patterns {
		pat = strings.ReplaceAll(pat, "\\", "/")
		if insensitive {
			pat = strings.ToLower(pat)
		}
		if strings.HasSuffix(pat, "/**") {
			prefix := strings.TrimSuffix(pat, "/**")
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return true
			}
			continue
		}
		if strings.Contains(pat, "/") {
			if ok, _ := filepath.Match(pat, p); ok {
				return true
			}
			continue
		}
		for _, c := range strings.Split(p, "/") {
			if ok, _ := filepath.Match(pat, c); ok {
				return true
			}
		}
	}
	return false
}
func addObservation(t, o map[string]any) {
	t["read_count"] = num(t["read_count"]) + 1
	reason := str(o["reason"])
	bucket := "full_read_count"
	if o["mode"] == "TARGETED" {
		bucket = "targeted_read_count"
	}
	for _, r := range []string{"DIRECTORY", "INVALID_INPUT", "MISSING_FILE", "OUTSIDE_ROOT", "PROTECTED_PATH", "RESOURCE_LIMIT", "STAT_UNAVAILABLE"} {
		if reason == r {
			bucket = "unknown_read_count"
		}
	}
	t[bucket] = num(t[bucket]) + 1
	a := t["observations"].([]any)
	if len(a) >= 128 {
		setReason(t, "OBSERVATION_LIMIT")
		return
	}
	a = append(a, o)
	t["observations"] = a
	setReason(t, reason)
}
