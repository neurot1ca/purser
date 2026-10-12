package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"purser-hook/internal/ledger"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const baseline, target, idleGap = 1.08, 1.5, 600.0

func main() {
	if err := run(os.Args[1:]); exitCode(err) != 0 {
		fmt.Fprint(os.Stderr, nativeText(err.Error()+"\n"))
		os.Exit(exitCode(err))
	}
}

type argumentError struct{ error }

func (e argumentError) Unwrap() error { return e.error }
func exitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	var parseErr argumentError
	if errors.As(err, &parseErr) {
		return 2
	}
	return 1
}
func projectSlug(root string) string {
	return regexp.MustCompile(`[^A-Za-z0-9-]`).ReplaceAllString(root, "-")
}
func transcriptGlob(root, home string) string {
	return filepath.Join(home, ".claude", "projects", projectSlug(root), "*.jsonl")
}

type session struct {
	name     string
	requests int
	totals   map[string]int64
	first    int64
	tools    map[int]int
	names    map[string]int
	amp      map[string]float64
	order    []string
	gaps     []float64
}
type event struct {
	requests int
	name     string
	size     float64
}
type summary struct {
	session
	sessions int
	weighted map[string]float64
	contexts []int64
}

func emptySession() session {
	return session{totals: map[string]int64{"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}, tools: map[int]int{}, names: map[string]int{}, amp: map[string]float64{}}
}
func number(v any) int64          { n, _ := v.(json.Number); x, _ := n.Int64(); return x }
func str(v any) string            { s, _ := v.(string); return s }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any            { a, _ := v.([]any); return a }

// Only serialized length matters; object key order cannot affect it.
func payloadSize(v any, nested bool) int {
	switch x := v.(type) {
	case string:
		if nested {
			return utf8.RuneCountInString(jsonString(x))
		}
		return utf8.RuneCountInString(x)
	case []any:
		n := 2 + max(len(x)-1, 0)*2
		for _, v := range x {
			n += payloadSize(v, true)
		}
		return n
	case map[string]any:
		n := 2 + max(len(x)-1, 0)*2
		for k, v := range x {
			n += payloadSize(k, true) + 2 + payloadSize(v, true)
		}
		return n
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			v, _ := x.Float64()
			return len(pythonFloat(v))
		}
		if x.String() == "-0" {
			return 1
		}
		return len(x.String())
	default:
		b, _ := json.Marshal(v)
		return len(b)
	}
}
func jsonString(value string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.NewReplacer(`\u2028`, "\u2028", `\u2029`, "\u2029").Replace(strings.TrimSuffix(buffer.String(), "\n"))
}
func analyze(path string) (session, error) {
	s := emptySession()
	s.name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	seen, toolCalls, toolNames := map[string]bool{}, map[string]int{}, map[string]string{}
	var times []float64
	var events []event
	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadString('\n') // ReadString has no transcript line limit.
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		var record map[string]any
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		decodeErr := dec.Decode(&record)
		var extra any
		if decodeErr == nil && dec.Decode(&extra) == io.EOF {
			message := object(record["message"])
			key := str(record["requestId"])
			if key == "" {
				key = str(message["id"])
			}
			if str(message["role"]) == "assistant" && key != "" && !seen[key] {
				seen[key] = true
				usage := object(message["usage"])
				for k, field := range map[string]string{"input": "input_tokens", "output": "output_tokens", "cache_read": "cache_read_input_tokens", "cache_write": "cache_creation_input_tokens"} {
					s.totals[k] += number(usage[field])
				}
				if s.first == 0 {
					s.first = number(usage["input_tokens"]) + number(usage["cache_read_input_tokens"]) + number(usage["cache_creation_input_tokens"])
				}
				stamp := strings.ReplaceAll(str(record["timestamp"]), "Z", "+00:00")
				for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05Z07:00", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
					if moment, err := time.ParseInLocation(layout, stamp, time.Local); err == nil {
						times = append(times, float64(moment.UnixMicro())/1e6)
						break
					}
				}
			}
			for _, value := range list(message["content"]) {
				block := object(value)
				switch block["type"] {
				case "tool_use":
					name := str(block["name"])
					if _, ok := block["name"]; !ok {
						name = "unknown"
					}
					toolNames[str(block["id"])] = name
					s.names[name]++
					if key != "" {
						toolCalls[key]++
					}
				case "tool_result":
					name, ok := toolNames[str(block["tool_use_id"])]
					if !ok {
						name = "unknown"
					}
					events = append(events, event{requests: len(seen), name: name, size: float64(payloadSize(block["content"], false)) / 4})
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return s, readErr
		}
	}
	s.requests = len(seen)
	for _, n := range toolCalls {
		s.tools[n]++
	}
	for _, e := range events {
		if _, ok := s.amp[e.name]; !ok {
			s.order = append(s.order, e.name)
		}
		s.amp[e.name] += e.size * float64(max(s.requests-e.requests, 0))
	}
	sort.Float64s(times)
	for i := 1; i < len(times); i++ {
		gap := times[i] - times[i-1]
		if gap > 0 && gap < idleGap {
			s.gaps = append(s.gaps, gap)
		}
	}
	return s, nil
}
func collect(pattern string, limit int) ([]session, error) {
	files, _ := filepath.Glob(pattern)
	modified := map[string]time.Time{}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		modified[path] = info.ModTime()
	}
	sort.SliceStable(files, func(i, j int) bool { return modified[files[i]].After(modified[files[j]]) })
	if len(files) > limit {
		files = files[:limit]
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("No transcripts matched: %s", pattern)
	}
	out := make([]session, len(files))
	for i, p := range files {
		var err error
		out[i], err = analyze(p)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func aggregate(sessions []session) summary {
	s := summary{session: emptySession(), sessions: len(sessions), weighted: map[string]float64{}}
	for _, x := range sessions {
		s.requests += x.requests
		for k, v := range x.totals {
			s.totals[k] += v
		}
		for k, v := range x.tools {
			s.tools[k] += v
		}
		for k, v := range x.names {
			s.names[k] += v
		}
		for _, k := range x.order {
			if _, ok := s.amp[k]; !ok {
				s.order = append(s.order, k)
			}
			s.amp[k] += x.amp[k]
		}
		s.gaps = append(s.gaps, x.gaps...)
		if x.first != 0 {
			s.contexts = append(s.contexts, x.first)
		}
	}
	s.weighted["input"] = float64(s.totals["input"])
	s.weighted["cache_read"] = float64(s.totals["cache_read"]) * .1
	s.weighted["cache_write"] = float64(s.totals["cache_write"]) * 1.25
	s.weighted["output"] = float64(s.totals["output"]) * 5
	return s
}
func median[T int64 | float64](a []T) float64 {
	if len(a) == 0 {
		return 0
	}
	slices.Sort(a)
	if len(a)%2 == 1 {
		return float64(a[len(a)/2])
	}
	return (float64(a[len(a)/2-1]) + float64(a[len(a)/2])) / 2
}
func grouped(v any) string {
	s := fmt.Sprintf("%d", v)
	if f, ok := v.(float64); ok {
		s = fmt.Sprintf("%.0f", f)
	}
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
func report(s summary, sessions []session) {
	requests := max(s.requests, 1)
	cache := s.totals["cache_read"]
	if cache == 0 {
		cache = 1
	}
	calls, bearing, avg := toolMetrics(s.tools)
	fmt.Printf(nativeText("%s\nORCHESTRATION TOKEN AUDIT  (%d sessions, deduplicated by requestId)\n%s\n"), strings.Repeat("=", 72), s.sessions, strings.Repeat("=", 72))
	fmt.Print(nativeText("\n-- BATCHING RATIO (the headline metric) --\n"))
	keys := make([]int, 0, len(s.tools))
	for k := range s.tools {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		n := s.tools[k]
		fmt.Printf(nativeText("   %d tool call(s): %6d requests  %5.1f%%\n"), k, n, 100*float64(n)/float64(max(bearing, 1)))
	}
	fmt.Printf(nativeText("   average          : %.2f   (baseline %g, target %g)\n"), avg, baseline, target)
	verdict := "NOT MOVING -- the CLAUDE.md batching rule is not being applied"
	if avg >= target {
		verdict = "AT/ABOVE TARGET"
	} else if avg > baseline+.05 {
		verdict = "improving"
	}
	fmt.Printf(nativeText("   status           : %s\n"), verdict)
	if avg > 0 && bearing > 0 {
		removed := float64(bearing) - float64(calls)/target
		if removed > 0 {
			saved := removed * (float64(cache) / float64(requests))
			fmt.Printf(nativeText("   reaching %g would remove %s requests = %s cache-read tokens (%.1f%%)\n"), target, grouped(removed), grouped(saved), 100*saved/float64(cache))
		}
	}
	t := s.totals
	fmt.Printf(nativeText("\n-- GROUND TRUTH --\n   assistant requests      : %s\n   input (uncached)        : %s\n   output                  : %s\n   cache read              : %s\n   cache write             : %s\n   avg context per request : %s\n   cost of one round trip  : %s cache-read tokens\n"), grouped(requests), grouped(t["input"]), grouped(t["output"]), grouped(t["cache_read"]), grouped(t["cache_write"]), grouped((t["input"]+t["cache_read"]+t["cache_write"])/int64(requests)), grouped(cache/int64(requests)))
	wtotal := 0.0
	for _, k := range []string{"input", "cache_read", "cache_write", "output"} {
		wtotal += s.weighted[k]
	}
	if wtotal == 0 {
		wtotal = 1
	}
	fmt.Print(nativeText("\n-- BILLABLE WEIGHT (input 1x, cache_read 0.1x, cache_write 1.25x, output 5.0x) --\n"))
	for _, k := range []string{"cache_read", "cache_write", "output", "input"} {
		v := s.weighted[k]
		fmt.Printf(nativeText("   %-12s %16s  %5.1f%%\n"), k, grouped(v), 100*v/wtotal)
	}
	prefix := median(append([]int64(nil), s.contexts...))
	if prefix != 0 {
		fmt.Printf(nativeText("\n-- STATIC PREFIX --\n   median first-request context: %s tokens\n   re-read on every request    : %s = %.1f%% of cache_read\n"), grouped(prefix), grouped(prefix*float64(requests)), 100*prefix*float64(requests)/float64(cache))
	}
	fmt.Print(nativeText("\n-- TOOL RESULT COST (approximate: chars/4, weighted by later re-reads) --\n"))
	names := append([]string(nil), s.order...)
	sort.SliceStable(names, func(i, j int) bool { return s.amp[names[i]] > s.amp[names[j]] })
	for i, n := range names {
		if i == 10 {
			break
		}
		name := string([]rune(n)[:min(32, utf8.RuneCountInString(n))])
		fmt.Printf(nativeText("   %-32s %5d calls  amplified %14s  %5.2f%%\n"), name, s.names[n], grouped(s.amp[n]), 100*s.amp[n]/float64(cache))
	}
	if len(s.gaps) > 0 {
		sort.Float64s(s.gaps)
		sum := 0.0
		for _, v := range s.gaps {
			sum += v
		}
		fmt.Printf(nativeText("\n-- WALL CLOCK (gap between assistant requests, idle excluded) --\n   median %.1fs   mean %.1fs   p90 %.1fs\n   active time across these sessions: %.1fh\n"), median(append([]float64(nil), s.gaps...)), sum/float64(len(s.gaps)), s.gaps[int(float64(len(s.gaps))*.9)], sum/3600)
	}
	fmt.Print(nativeText("\n-- PER SESSION --\n"))
	for _, x := range sessions {
		_, _, v := toolMetrics(x.tools)
		name := string([]rune(x.name)[:min(8, utf8.RuneCountInString(x.name))])
		fmt.Printf(nativeText("   %s  requests=%5d  cache_read=%14s  tools/req=%.2f\n"), name, x.requests, grouped(x.totals["cache_read"]), v)
	}
}
func run(args []string) error {
	fs := flag.NewFlagSet("purser-audit", flag.ContinueOnError)
	sessions := fs.Int("sessions", 15, "Number of most recent transcripts to read (default: 15).")
	pattern := fs.String("transcripts", "", "Glob for session .jsonl transcripts.")
	root := fs.String("project-root", "", "Governed project root (default: git top-level of the current directory).")
	jsonOut := fs.String("json", "", "Also write the summary to this path as JSON")
	if err := fs.Parse(args); err != nil {
		return argumentError{err}
	}
	if *sessions < 1 {
		return errors.New("--sessions must be >= 1")
	}
	if fs.NArg() != 0 {
		return argumentError{fmt.Errorf("unrecognized arguments: %s", strings.Join(fs.Args(), " "))}
	}
	resolved, err := resolveRoot(*root)
	if err != nil {
		return err
	}
	if *pattern == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*pattern = transcriptGlob(resolved, home)
	}
	data, err := collect(*pattern, *sessions)
	if err != nil {
		return err
	}
	s := aggregate(data)
	report(s, data)
	if *jsonOut != "" {
		weighted := map[string]json.Number{}
		for k, v := range s.weighted {
			weighted[k] = pythonFloat(v)
		}
		amplified := map[string]json.Number{}
		for name, value := range s.amp {
			amplified[name] = pythonFloat(value)
		}
		out := map[string]any{
			"sessions":              s.sessions,
			"requests":              s.requests,
			"totals":                s.totals,
			"weighted":              weighted,
			"tools_per_request":     s.tools,
			"tools_per_request_avg": pythonFloat(average(s.tools)),
			"batching_baseline":     pythonFloat(baseline),
			"batching_target":       pythonFloat(target),
			"median_first_context":  medianJSON(s.contexts),
			"amplified_tool_tokens": amplified,
			"tool_call_counts":      s.names,
		}
		b := marshalIndentedJSON(out)
		if e := os.WriteFile(*jsonOut, b, 0644); e != nil {
			return e
		}
		fmt.Printf(nativeText("\nwrote %s\n"), *jsonOut)
	}
	return nil
}
func pythonFloat(value float64) json.Number {
	format := byte('g')
	if value == 0 || math.Abs(value) >= 1e-4 && math.Abs(value) < 1e16 {
		format = 'f'
	}
	text := strconv.FormatFloat(value, format, -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return json.Number(text)
}
func medianJSON(values []int64) any {
	if len(values) == 0 {
		return int64(0)
	}
	slices.Sort(values)
	if len(values)%2 == 1 {
		return values[len(values)/2]
	}
	return pythonFloat(median(values))
}
func marshalIndentedJSON(value any) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
	var ascii strings.Builder
	for _, r := range strings.TrimSuffix(buffer.String(), "\n") {
		if r < 127 {
			ascii.WriteRune(r)
		} else if r <= 0xffff {
			fmt.Fprintf(&ascii, `\u%04x`, r)
		} else {
			r -= 0x10000
			fmt.Fprintf(&ascii, `\u%04x\u%04x`, 0xd800+r/1024, 0xdc00+r%1024)
		}
	}
	return []byte(nativeText(ascii.String()))
}
func toolMetrics(counts map[int]int) (calls, bearing int, avg float64) {
	for n, count := range counts {
		calls += n * count
		bearing += count
	}
	if bearing != 0 {
		avg = float64(calls) / float64(bearing)
	}
	return
}
func average(counts map[int]int) float64 { _, _, avg := toolMetrics(counts); return avg }
func resolveRoot(explicit string) (string, error) {
	if explicit != "" {
		return resolvePath(explicit)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", errors.New("project root unresolved: cwd is invalid")
	}
	root, err := ledger.GitTopLevel(cwd)
	if err == nil {
		return root, nil
	}
	var failure *ledger.InspectionError
	reason := "git top-level lookup failed"
	if errors.As(err, &failure) {
		if mapped := map[string]string{"GIT_UNAVAILABLE": "git executable is unavailable", "GIT_FAILED": "cwd is not inside a git work tree", "GIT_OUTPUT_INVALID": "git returned an invalid top-level"}[failure.Code]; mapped != "" {
			reason = mapped
		}
	}
	return "", fmt.Errorf("project root unresolved: %s", reason)
}
func resolvePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if !os.IsNotExist(err) {
		return real, err
	}
	if parent := filepath.Dir(absolute); parent != absolute {
		real, err = resolvePath(parent)
		return filepath.Join(real, filepath.Base(absolute)), err
	}
	return absolute, nil
}
func nativeText(value string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(value, "\n", "\r\n")
	}
	return value
}
