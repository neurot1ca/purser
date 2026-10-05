package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Record keeps top-level fields in their original order and retains raw values,
// matching Python dict insertion order and json.dump's compact value encoding.
type Record struct {
	keys     []string
	values   map[string]json.RawMessage
	original []byte
	dirty    bool
}

func Decode(data []byte) (*Record, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("job record must be a JSON object")
	}
	r := &Record{values: make(map[string]json.RawMessage), original: append([]byte(nil), data...)}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, errors.New("invalid job field")
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		if _, exists := r.values[key]; !exists {
			r.keys = append(r.keys, key)
		}
		r.values[key] = raw
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON data")
		}
		return nil, err
	}
	return r, nil
}

func Read(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

func (r *Record) Get(key string, dst any) bool {
	b, ok := r.values[key]
	if !ok {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}

func (r *Record) Set(key string, value any) error {
	b, err := pythonJSON(value)
	if err != nil {
		return err
	}
	if _, ok := r.values[key]; !ok {
		r.keys = append(r.keys, key)
	}
	r.values[key] = b
	r.dirty = true
	return nil
}

func (r *Record) Delete(key string) {
	if _, ok := r.values[key]; !ok {
		return
	}
	delete(r.values, key)
	for i, existing := range r.keys {
		if existing == key {
			r.keys = append(r.keys[:i], r.keys[i+1:]...)
			break
		}
	}
	r.dirty = true
}

func (r *Record) Bytes() ([]byte, error) {
	if !r.dirty {
		return append([]byte(nil), r.original...), nil
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range r.keys {
		if i > 0 {
			out.WriteString(", ")
		}
		k, err := pythonJSON(key)
		if err != nil {
			return nil, err
		}
		out.Write(k)
		out.WriteString(": ")
		out.Write(r.values[key])
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func pythonJSON(v any) ([]byte, error) {
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	b := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	var plain bytes.Buffer
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+1 < len(b) {
			if b[i+1] == '\\' {
				plain.Write(b[i : i+2])
				i++
				continue
			}
			if i+5 < len(b) && (string(b[i:i+6]) == `\u2028` || string(b[i:i+6]) == `\u2029`) {
				if b[i+5] == '8' {
					plain.WriteString(" ")
				} else {
					plain.WriteString(" ")
				}
				i += 5
				continue
			}
		}
		plain.WriteByte(b[i])
	}
	return plain.Bytes(), nil
}

// Write atomically replaces a Python-compatible JSON record without adding a newline.
func Write(path string, r *Record) error {
	b, err := r.Bytes()
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}

var replaceFile = os.Rename

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = replaceFile(tmp, path)
		if err == nil || !transientReplace(err) || attempt == 4 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
}

func transientReplace(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.EACCES || errno == syscall.EBUSY || errno == syscall.Errno(32)
}

var ErrClaimConflict = errors.New("job generation claim conflict")

type ClaimConflictError struct {
	Expected int64
	Actual   int64
}

func (e *ClaimConflictError) Error() string {
	return fmt.Sprintf("%v: expected generation %d, found %d", ErrClaimConflict, e.Expected, e.Actual)
}
func (e *ClaimConflictError) Unwrap() error { return ErrClaimConflict }

// ClaimGeneration reserves a queued generation by atomically moving it to RUNNING.
// The ownership token and generation are checked under the interoperable owner lock.
func ClaimGeneration(path string, generation int64, token string) (*Record, error) {
	jobID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	lock, err := AcquireOwnership(filepath.Dir(path), jobID)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	r, err := Read(path)
	if err != nil {
		return nil, err
	}
	var actual int64
	var owner, state string
	r.Get("generation", &actual)
	r.Get("ownershipToken", &owner)
	r.Get("state", &state)
	if actual != generation || owner != token || state != "QUEUED" {
		return nil, &ClaimConflictError{Expected: generation, Actual: actual}
	}
	if err = r.Set("state", "RUNNING"); err != nil {
		return nil, err
	}
	if err = Write(path, r); err != nil {
		return nil, err
	}
	return r, nil
}

// WriteFenced refuses to persist a stale host's update after a newer generation owns the job.
func WriteFenced(path string, r *Record, generation int64, token string) error {
	jobID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	lock, err := AcquireOwnership(filepath.Dir(path), jobID)
	if err != nil {
		return err
	}
	defer lock.Release()
	current, err := Read(path)
	if err != nil {
		return err
	}
	var actual int64
	var owner string
	current.Get("generation", &actual)
	current.Get("ownershipToken", &owner)
	if actual != generation || owner != token {
		return &ClaimConflictError{Expected: generation, Actual: actual}
	}
	return Write(path, r)
}
