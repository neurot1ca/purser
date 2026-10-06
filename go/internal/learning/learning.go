package learning

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"purser-hook/internal/contextio"
)

const (
	SummaryName      = "learning_summary.json"
	LockName         = "learning_summary.lock"
	MaxSummaryBytes  = 4 * 1024 * 1024
	MaxTaskRecords   = 1024
	MaxTaskBytes     = 2048
	MaxCohorts       = 256
	MaxSamples       = 64
	MaxDistillPerRun = 512
	MaxSourceIDs     = 16384
)

type Source struct {
	Path string
	Job  map[string]any
}

type Summary = map[string]any

func summaryPath(root string) string { return filepath.Join(root, "telemetry", SummaryName) }

func SourceKey(identity string) string {
	if len(identity) == 64 {
		valid := true
		for _, r := range identity {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				valid = false
				break
			}
		}
		if valid {
			return identity
		}
	}
	h := sha256.Sum256([]byte(identity))
	return "opaque:" + hex.EncodeToString(h[:])
}

func empty() Summary {
	return Summary{"schema_version": 1, "sources": []any{}, "records": []any{}, "cohorts": map[string]any{}}
}

func ReadSummary(stateRoot string) Summary {
	b, err := os.ReadFile(summaryPath(stateRoot))
	if err != nil {
		return empty()
	}
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&value) != nil || numInt(value["schema_version"]) != 1 {
		return empty()
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return empty()
	}
	if _, ok := value["sources"].([]any); !ok {
		return empty()
	}
	if _, ok := value["records"].([]any); !ok {
		return empty()
	}
	if _, ok := value["cohorts"].(map[string]any); !ok {
		return empty()
	}
	return value
}

func DistillJobs(stateRoot string, sources []Source, now float64, maxSources int, existingSources map[string]bool) (map[string]bool, error) {
	path := summaryPath(stateRoot)
	unlock, err := contextio.LockFile(filepath.Join(filepath.Dir(path), LockName))
	if err != nil {
		return nil, err
	}
	defer unlock()
	d := ReadSummary(stateRoot)
	present := existingSources
	if present == nil {
		present = map[string]bool{}
		for _, src := range sources {
			if _, err := os.Stat(src.Path); err == nil {
				present[SourceKey(strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path)))] = true
			}
		}
	}
	old := asStrings(d["sources"])
	ids := map[string]bool{}
	for _, id := range old {
		if present[id] {
			ids[id] = true
		}
	}
	d["sources"] = stringsAny(sortedBoolKeys(ids))
	records, _ := d["records"].([]any)
	recorded := map[string]bool{}
	for _, value := range records {
		if record, ok := value.(map[string]any); ok {
			if id, ok := record["id"].(string); ok {
				recorded[id] = true
			}
		}
	}
	limit := maxSources
	if limit > MaxDistillPerRun {
		limit = MaxDistillPerRun
	}
	if limit < 0 {
		limit = 0
	}
	processed := 0
	for _, src := range sources {
		stem := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
		id := SourceKey(stem)
		if processed >= limit || len(ids) >= MaxSourceIDs {
			break
		}
		if ids[id] {
			continue
		}
		stamp := src.Job["finishedAt"]
		state, _ := src.Job["state"].(string)
		var record map[string]any
		if terminal(state) && validNumber(stamp) && id == stem {
			record = makeRecord(src.Job, task(stateRoot, src.Job), stamp)
		}
		if record != nil && !recorded[id] {
			mergeRecord(d, record)
			records = append(records, record)
			if len(records) > MaxTaskRecords {
				records = records[len(records)-MaxTaskRecords:]
			}
			recorded[id] = true
		}
		ids[id] = true
		processed++
	}
	d["records"] = records
	d["sources"] = stringsAny(sortedBoolKeys(ids))
	b, err := encode(d)
	if err != nil {
		return nil, err
	}
	if err = replaceCheckpoint(path, b); err != nil {
		return nil, err
	}
	return ids, nil
}

func IsDistilled(stateRoot, identity string) bool {
	for _, id := range asStrings(ReadSummary(stateRoot)["sources"]) {
		if id == identity {
			return true
		}
	}
	return false
}

func terminal(s string) bool {
	switch s {
	case "SUCCEEDED", "FAILED", "CANCELLED", "TIMED_OUT", "EXPIRED":
		return true
	}
	return false
}
func task(root string, job map[string]any) map[string]any {
	taskID, _ := job["taskId"].(string)
	jobID, _ := job["jobId"].(string)
	if taskID == "" {
		return map[string]any{}
	}
	b, err := os.ReadFile(filepath.Join(root, "async_jobs", jobID+".json"))
	if err != nil {
		return map[string]any{}
	}
	var idx map[string]any
	if json.Unmarshal(b, &idx) != nil {
		return map[string]any{}
	}
	project, ok := idx["project_key"].(string)
	if !ok {
		return map[string]any{}
	}
	// Telemetry task snapshots are the aggregate produced by the Python telemetry builder.
	projectRoot := filepath.Join(root, "projects", project, "telemetry")
	b, err = os.ReadFile(filepath.Join(projectRoot, "tasks", taskID+".json"))
	if err != nil {
		return map[string]any{}
	}
	var result map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&result) != nil || result["project_key"] != project {
		return map[string]any{}
	}
	// Python rebuilds these decision counters from the task index on every pass.
	index, err := os.Open(filepath.Join(root, "telemetry", "events", "by_task", taskID+".jsonl"))
	if err == nil {
		defer index.Close()
		var valid, falseCount float64
		scanner := bufio.NewScanner(index)
		scanner.Buffer(make([]byte, 4096), MaxSummaryBytes)
		for scanner.Scan() {
			var event map[string]any
			dec := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
			dec.UseNumber()
			if dec.Decode(&event) != nil || event["task_id"] != taskID || event["project_key"] != project || event["event_type"] != "CLAUDE_DECISION_RECORDED" {
				continue
			}
			payload, _ := event["payload"].(map[string]any)
			if validNumber(payload["reviewer1_valid_count"]) {
				valid += numberFloat(payload["reviewer1_valid_count"])
			}
			if validNumber(payload["reviewer1_false_count"]) {
				falseCount += numberFloat(payload["reviewer1_false_count"])
			}
		}
		if valid != 0 {
			result["reviewer1_valid_count"] = valid
		}
		if falseCount != 0 {
			result["reviewer1_false_count"] = falseCount
		}
	}
	return result
}

func makeRecord(job, task map[string]any, stamp any) map[string]any {
	acceptance, _ := task["claude_acceptance"].(map[string]any)
	review, _ := job["reviewerOutcome"].(string)
	skipped, _ := task["reviewer1_skipped_reason"].(string)
	result, hasResult := task["reviewer1_result"]
	if review != "passed" && review != "failed" && review != "skipped" && review != "unavailable" {
		switch {
		case skipped == "REVIEWER1_UNAVAILABLE":
			review = "unavailable"
		case skipped != "":
			review = "skipped"
		case hasResult && result != nil:
			if result == "APPROVED" || result == "PASS" || result == "PASSED" {
				review = "passed"
			} else {
				review = "failed"
			}
		default:
			review = ""
		}
	}
	duration := task["duration_ms"]
	if !validNumber(duration) {
		duration = job["durationMs"]
	}
	if !validNumber(duration) {
		duration = nil
	}
	etaError := any(nil)
	if validNumber(duration) && validNumber(job["etaMinutes"]) {
		etaError = numberFloat(duration) - numberFloat(job["etaMinutes"])*60000
	}
	cohort := []any{fallback(task["project_key"], job["project_key"], "unknown"), fallback(job["route"], task["route"], nil), fallback(job["jobModel"], task["model"], nil), fallback(job["jobEffort"], task["effort"], nil)}
	parts := make([]string, 4)
	for i, p := range cohort {
		if p == nil {
			parts[i] = "unknown"
		} else {
			parts[i] = truncate(fmt.Sprint(p), 64)
		}
	}
	usage, _ := task["usage"].(map[string]any)
	if usage == nil {
		usage, _ = job["usage"].(map[string]any)
	}
	var usageComplete any
	for _, key := range []string{"claude_task_usage_complete", "claude_usage_known", "codex_usage_known", "gemini_usage_known", "deepseek_usage_known"} {
		if v, ok := usage[key].(bool); ok {
			usageComplete = v
			break
		}
	}
	quota := task["quota_samples"]
	if quota == nil {
		quota = job["quota_samples"]
	}
	if quota == nil && usage != nil {
		q := []any{}
		for _, w := range []string{"5h", "7d"} {
			for _, edge := range []string{"start", "end"} {
				pct := usage["claude_"+w+"_"+edge+"_pct"]
				if pct != nil {
					q = append(q, map[string]any{"timestamp": usage["_claude_"+w+"_"+edge+"_captured_at"], "resets_at": usage["_claude_"+w+"_"+edge+"_resets_at"], "used": pct})
				}
			}
		}
		quota = q
	}
	quota = normalizeQuota(quota)
	r := map[string]any{"id": job["jobId"], "task": job["taskId"], "at": stamp, "cohort": strings.Join(parts, "|"), "duration_ms": duration, "eta_error_ms": etaError, "acceptance": acceptance["outcome"], "send_backs": sendbacks(acceptance), "review": nonempty(review), "reviewer1_valid": fallback(job["reviewer1Valid"], task["reviewer1_valid_count"], nil), "reviewer1_false": fallback(job["reviewer1False"], task["reviewer1_false_count"], nil), "validation": fallback(task["validation"], job["validation"], nil), "apply_conflict": fallback(job["applyConflictReason"], conflict(job), nil), "failure": job["errorCode"], "usage": usage, "usage_complete": usageComplete, "quota": quota}
	for k, v := range r {
		if v == nil {
			delete(r, k)
		}
	}
	for len(pythonBytes(r)) > MaxTaskBytes {
		if u, ok := r["usage"].(map[string]any); ok && len(u) > 0 {
			keys := sortedKeys(u)
			delete(u, keys[0])
		} else if q, ok := r["quota"].([]any); ok && len(q) > 0 {
			r["quota"] = q[1:]
		} else {
			trimmed := false
			for _, k := range []string{"task", "cohort", "id"} {
				if s, ok := r[k].(string); ok && len(s) > 32 {
					r[k] = truncate(s, 32)
					trimmed = true
					break
				}
			}
			if !trimmed {
				break
			}
		}
	}
	return r
}

func cell() map[string]any {
	return map[string]any{"counts": map[string]any{"tasks": 0, "acceptance": map[string]any{}, "send_backs": nil, "review": map[string]any{}, "validation": map[string]any{}, "reviewer1_valid": nil, "reviewer1_false": nil, "apply_conflicts": map[string]any{}, "failures": map[string]any{}, "token_usage": map[string]any{}, "token_usage_complete": map[string]any{}}, "samples": []any{}, "quota_samples": []any{}}
}
func mergeRecord(data, record map[string]any) {
	cohorts := data["cohorts"].(map[string]any)
	key := record["cohort"].(string)
	if _, ok := cohorts[key]; !ok && len(cohorts) >= MaxCohorts-1 {
		key = "other"
	}
	c, ok := cohorts[key].(map[string]any)
	if !ok {
		c = cell()
		cohorts[key] = c
	}
	counts := c["counts"].(map[string]any)
	counts["tasks"] = numberFloat(counts["tasks"]) + 1
	for _, pair := range [][2]string{{"acceptance", "acceptance"}, {"review", "review"}, {"validation", "validation"}, {"apply_conflict", "apply_conflicts"}, {"failure", "failures"}} {
		countValue(counts[pair[1]].(map[string]any), record[pair[0]])
	}
	if validNumber(record["send_backs"]) {
		counts["send_backs"] = numberFloat(counts["send_backs"]) + numberFloat(record["send_backs"])
	}
	for _, f := range []string{"reviewer1_valid", "reviewer1_false"} {
		if validNumber(record[f]) {
			counts[f] = numberFloat(counts[f]) + numberFloat(record[f])
		}
	}
	if u, ok := record["usage"].(map[string]any); ok {
		for k, v := range u {
			if strings.HasSuffix(k, "tokens") && validNumber(v) {
				dest := counts["token_usage"].(map[string]any)
				name := k
				if _, ok := dest[name]; !ok && len(dest) >= 31 {
					name = "other"
				}
				dest[name] = numberFloat(dest[name]) + numberFloat(v)
			}
		}
	}
	if b, ok := record["usage_complete"].(bool); ok {
		dest := counts["token_usage_complete"].(map[string]any)
		k := "false"
		if b {
			k = "true"
		}
		dest[k] = numberFloat(dest[k]) + 1
	}
	samples := c["samples"].([]any)
	sample := map[string]any{}
	for _, k := range []string{"at", "duration_ms", "eta_error_ms"} {
		if v, ok := record[k]; ok {
			sample[k] = v
		}
	}
	if sample["duration_ms"] != nil || sample["eta_error_ms"] != nil {
		samples = append(samples, sample)
		if len(samples) > MaxSamples {
			samples = samples[len(samples)-MaxSamples:]
		}
	}
	c["samples"] = samples
	if q, ok := record["quota"].([]any); ok {
		v := append(c["quota_samples"].([]any), q...)
		if len(v) > MaxSamples {
			v = v[len(v)-MaxSamples:]
		}
		c["quota_samples"] = v
	}
}

func countValue(m map[string]any, v any) {
	s, ok := v.(string)
	if !ok || s == "" {
		return
	}
	k := truncate(s, 64)
	if _, ok = m[k]; !ok && len(m) >= 31 {
		k = "other"
	}
	m[k] = numberFloat(m[k]) + 1
}

func encode(data Summary) ([]byte, error) {
	for {
		b := pythonBytes(data)
		if len(b) <= MaxSummaryBytes {
			return b, nil
		}
		records := data["records"].([]any)
		if len(records) > 0 {
			data["records"] = records[1:]
			continue
		}
		cohorts := data["cohorts"].(map[string]any)
		if len(cohorts) == 0 {
			return nil, errors.New("learning checkpoint metadata exceeds its byte cap")
		}
		keys := sortedKeys(cohorts)
		largest := keys[0]
		size := -1
		for _, k := range keys {
			n := len(pythonBytes(cohorts[k]))
			if n > size {
				largest, size = k, n
			}
		}
		c := cohorts[largest].(map[string]any)
		counts := c["counts"].(map[string]any)
		changed := false
		for _, name := range []string{"tasks", "acceptance", "send_backs", "review", "validation", "reviewer1_valid", "reviewer1_false", "apply_conflicts", "failures", "token_usage", "token_usage_complete"} {
			m, ok := counts[name].(map[string]any)
			if !ok || len(m) < 2 {
				continue
			}
			for _, k := range orderedKeys(m) {
				if k == "other" {
					continue
				}
				v := m[k]
				delete(m, k)
				if validNumber(v) {
					m["other"] = numberFloat(m["other"]) + numberFloat(v)
				}
				changed = true
				break
			}
			if changed {
				break
			}
		}
		if !changed {
			for _, name := range []string{"samples", "quota_samples"} {
				values := c[name].([]any)
				if len(values) > 1 {
					n := len(values) / 2
					c[name] = values[n:]
					changed = true
					break
				}
			}
		}
		if !changed {
			return nil, errors.New("learning checkpoint cells exceed their byte cap")
		}
	}
}

// Python's json.dumps(..., separators=(",", ":"), ensure_ascii=True) keeps
// insertion order and escapes non-ASCII without escaping HTML characters.
func pythonBytes(v any) []byte { var b bytes.Buffer; writePython(&b, v); return b.Bytes() }
func writePython(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, x)
	case json.Number:
		b.WriteString(pythonNumber(string(x)))
	case int:
		b.WriteString(fmt.Sprint(x))
	case int64:
		b.WriteString(fmt.Sprint(x))
	case float64:
		formatted := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(formatted, ".eE") {
			formatted += ".0"
		}
		b.WriteString(pythonNumber(formatted))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writePython(b, e)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteByte('{')
		for i, k := range orderedKeys(x) {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writePython(b, x[k])
		}
		b.WriteByte('}')
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			b.WriteString("null")
		} else {
			var generic any
			d := json.NewDecoder(bytes.NewReader(raw))
			d.UseNumber()
			if d.Decode(&generic) == nil {
				writePython(b, generic)
			} else {
				b.Write(raw)
			}
		}
	}
}

var keyOrder = []string{"schema_version", "sources", "records", "cohorts", "id", "task", "at", "cohort", "duration_ms", "eta_error_ms", "acceptance", "send_backs", "review", "reviewer1_valid", "reviewer1_false", "validation", "apply_conflict", "failure", "usage", "usage_complete", "quota", "timestamp", "resets_at", "used", "limit", "counts", "tasks", "send_backs", "reviewer1_valid", "reviewer1_false", "apply_conflicts", "failures", "token_usage", "token_usage_complete", "samples", "quota_samples"}

func orderedKeys(m map[string]any) []string {
	if _, ok := m["tasks"]; ok {
		if _, ok := m["acceptance"]; ok {
			order := []string{"tasks", "acceptance", "send_backs", "review", "validation", "reviewer1_valid", "reviewer1_false", "apply_conflicts", "failures", "token_usage", "token_usage_complete"}
			keys := make([]string, 0, len(m))
			used := map[string]bool{}
			for _, key := range order {
				if _, ok := m[key]; ok {
					keys = append(keys, key)
					used[key] = true
				}
			}
			for _, key := range sortedKeys(m) {
				if !used[key] {
					keys = append(keys, key)
				}
			}
			return keys
		}
	}
	keys := make([]string, 0, len(m))
	used := map[string]bool{}
	for _, want := range keyOrder {
		if _, ok := m[want]; ok && !used[want] {
			keys = append(keys, want)
			used[want] = true
		}
	}
	rest := make([]string, 0, len(m)-len(keys))
	for k := range m {
		if !used[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else if r > 0xffff {
				r -= 0x10000
				fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			} else if r > 0x7f {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}
func pythonNumber(s string) string {
	if strings.ContainsAny(s, "eE") {
		parts := strings.FieldsFunc(s, func(r rune) bool { return r == 'e' || r == 'E' })
		if len(parts) == 2 {
			exp, err := strconv.Atoi(parts[1])
			if err == nil {
				sign := ""
				if exp >= 0 {
					sign = "+"
				} else {
					sign = "-"
					exp = -exp
				}
				return parts[0] + "e" + sign + fmt.Sprintf("%02d", exp)
			}
		}
	}
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	return s
}

func replaceCheckpoint(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, e := os.Open(dir)
		if e != nil {
			return e
		}
		err = d.Sync()
		ce := d.Close()
		if err != nil {
			return err
		}
		if ce != nil {
			return ce
		}
	}
	return nil
}

func validNumber(v any) bool {
	switch x := v.(type) {
	case int, int64, float32, float64, json.Number:
		return !(isNaN(x))
	default:
		return false
	}
}
func isNaN(v any) bool { f := numberFloat(v); return math.IsNaN(f) || math.IsInf(f, 0) }
func numInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case float64:
		return int(n)
	}
	return 0
}
func numberFloat(v any) float64 {
	switch n := v.(type) {
	case nil:
		return 0
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
func fallback(a, b, c any) any {
	if a != nil {
		return a
	}
	if b != nil {
		return b
	}
	return c
}
func conflict(job map[string]any) any {
	if job["applyState"] == "conflict" {
		return "conflict"
	}
	return nil
}
func sendbacks(a map[string]any) any {
	if a == nil {
		return nil
	}
	if a["outcome"] == "accepted" || a["outcome"] == "rejected" || validNumber(a["send_backs"]) && numberFloat(a["send_backs"]) > 0 {
		return a["send_backs"]
	}
	return nil
}
func nonempty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func normalizeQuota(v any) any {
	if one, ok := v.(map[string]any); ok {
		v = []any{one}
	}
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	if len(items) > MaxSamples {
		items = items[len(items)-MaxSamples:]
	}
	out := []any{}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok || m["timestamp"] == nil || m["resets_at"] == nil {
			continue
		}
		q := map[string]any{}
		for _, k := range []string{"timestamp", "resets_at", "used", "limit"} {
			if value, ok := m[k]; ok {
				if s, ok := value.(string); ok {
					q[k] = truncate(s, 64)
				} else {
					q[k] = value
				}
			}
		}
		out = append(out, q)
	}
	return out
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func stringsAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
func asStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, x := range items {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
