package enforce

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"purser-hook/internal/contextio"
	"purser-hook/internal/obsidian"
)

const observationSchemaVersion = 1

var observationNow = time.Now

const (
	observationMarkerPrefix     = "<!-- orchestrator projection v1 sha256="
	observationYAMLMarkerPrefix = "# orchestrator projection v1 sha256="
)

var errObservationRunNoteUndated = errors.New("run note has no anchor time")

var observationMarker = regexp.MustCompile(`(?m)^(?:<!-- orchestrator projection v1 sha256=([0-9a-f]{64}) -->|# orchestrator projection v1 sha256=([0-9a-f]{64}))\n`)

// observationProjectionSettings mirrors telemetry._projection_settings and
// obsidian_projection._vault_root. Invalid or absent settings disable output.
func observationProjectionSettings(configData []byte, envVault, explicitVault *string) (bool, string) {
	return obsidian.ProjectionSettings(configData, envVault, explicitVault)
}

func observationAbsoluteVault(value string) string {
	if strings.TrimSpace(value) == "" || !filepath.IsAbs(value) {
		return ""
	}
	return filepath.Clean(value)
}

// observationResolvedPath follows existing symlinks while allowing missing
// suffixes, matching pathlib.Path.resolve()'s default strict=False behavior.
func observationResolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return observationResolvedPathDepth(abs, 0)
}

func observationResolvedPathDepth(abs string, depth int) (string, error) {
	if depth > 32 {
		return "", fmt.Errorf("too many path links")
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(filepath.Separator)
	rest := strings.TrimLeft(strings.TrimPrefix(abs, volume), `\\/`)
	parts := strings.Split(rest, string(filepath.Separator))
	for index, part := range parts {
		if part == "" || part == "." {
			continue
		}
		candidate := filepath.Join(current, part)
		if target, err := os.Readlink(candidate); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(candidate), target)
			}
			resolved, resolveErr := observationResolvedPathDepth(filepath.Clean(target), depth+1)
			if resolveErr != nil {
				return "", resolveErr
			}
			current = resolved
		} else {
			if _, statErr := os.Lstat(candidate); statErr != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					return "", statErr
				}
				for _, suffix := range parts[index:] {
					if suffix != "" && suffix != "." {
						current = filepath.Join(current, suffix)
					}
				}
				return filepath.Clean(current), nil
			}
			current = candidate
		}
	}
	return filepath.Clean(current), nil
}

func observationWithin(root, candidate string) bool {
	r, err := observationResolvedPath(root)
	if err != nil {
		return false
	}
	c, err := observationResolvedPath(candidate)
	if err != nil {
		return false
	}
	return observationRelativeWithin(r, c)
}

func observationRelativeWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func observationSafeVaultPath(root string, parts ...string) (string, bool) {
	candidate := root
	for _, part := range parts {
		if filepath.IsAbs(part) {
			candidate = part
		} else {
			candidate = filepath.Join(candidate, part)
		}
	}
	return candidate, observationWithin(root, candidate)
}

func observationSafeRunPath(root, year, month, taskID string) (string, bool) {
	runs := filepath.Join(root, "Runs")
	candidate := filepath.Join(runs, year, month, taskID+".md")
	resolvedRoot, err := observationResolvedPath(root)
	if err != nil {
		return candidate, false
	}
	resolvedRuns, err := observationResolvedPath(runs)
	if err != nil {
		return candidate, false
	}
	resolvedCandidate, err := observationResolvedPath(candidate)
	if err != nil {
		return candidate, false
	}
	return candidate, observationRelativeWithin(resolvedRoot, resolvedRuns) && observationRelativeWithin(resolvedRoot, resolvedCandidate) && observationRelativeWithin(resolvedRuns, resolvedCandidate)
}

func observationMarkerFor(content string, yamlComment bool) string {
	digest := sha256.Sum256([]byte(content))
	prefix := observationMarkerPrefix
	suffix := " -->"
	if yamlComment {
		prefix, suffix = observationYAMLMarkerPrefix, ""
	}
	return prefix + hex.EncodeToString(digest[:]) + suffix + "\n"
}

func observationWithMarker(prefix, body string, yamlComment bool) string {
	return prefix + observationMarkerFor(prefix+body, yamlComment) + body
}

// renderObservationRunNote mirrors obsidian_projection.render_run_note and
// project_task's path calculation. It renders only; callers own all IO.
func renderObservationRunNote(aggregate map[string]any, vaultRoot string) (string, string, error) {
	if aggregate["started_at"] == nil && aggregate["ended_at"] == nil {
		return "", "", errObservationRunNoteUndated
	}
	p, err := prepareObservationRun(aggregate)
	if err != nil {
		return "", "", err
	}
	zone, _ := observationResolveDisplayZone()
	year, month := "0000", "00"
	anchor := p.started
	if anchor == nil {
		anchor = p.ended
	}
	if anchor == nil {
		return "", "", errObservationRunNoteUndated
	}
	local := anchor.In(zone)
	year, month = fmt.Sprintf("%04d", local.Year()), fmt.Sprintf("%02d", local.Month())
	path, ok := observationSafeRunPath(vaultRoot, year, month, p.taskID)
	if !ok {
		return "", "", errors.New("run note path escaped the Runs tree")
	}
	fm := observationRunFrontmatter(p)
	body := observationRunBody(p)
	return observationWithMarker(fm, body, false), path, nil
}

func cleanupObservationUndatedRunNotes(vaultRoot string) {
	placeholder, ok := observationSafeRunPath(vaultRoot, "0000", "00", "cleanup")
	if !ok {
		return
	}
	dir := filepath.Dir(placeholder)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		taskID := strings.TrimSuffix(name, ".md")
		candidate, safe := observationSafeRunPath(vaultRoot, "0000", "00", taskID)
		if !safe {
			continue
		}
		content, readErr := os.ReadFile(candidate)
		if readErr == nil && bytes.Contains(content, []byte(observationMarkerPrefix)) {
			_ = os.Remove(candidate)
		}
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return
	}
	if !observationWithin(filepath.Join(vaultRoot, "Runs"), dir) {
		return
	}
	_ = os.Remove(dir)
	parent := filepath.Dir(dir)
	if !observationWithin(filepath.Join(vaultRoot, "Runs"), parent) {
		return
	}
	entries, err = os.ReadDir(parent)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(parent)
	}
}

// renderObservationHealthNote mirrors project_health's read and render path.
// It renders only; callers own all writes.
func renderObservationHealthNote(stateRoot, vaultRoot string, lastProjectionWriteAt *string) (string, string, error) {
	path, ok := observationSafeVaultPath(vaultRoot, "System", "Telemetry Health.md")
	if !ok {
		return "", "", errors.New("projection path escaped the vault root")
	}
	root := filepath.Join(stateRoot, "telemetry")
	health := map[string]any{"status": nil, "failure_count": nil, "last_error_code": nil, "updated_at": nil, "last_projection_write_at": nil}
	if data, err := os.ReadFile(filepath.Join(root, "telemetry_health.json")); err == nil {
		if health, err = decodeObservationHealthObject(data); err != nil {
			return "", "", errors.New("telemetry health is unreadable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("telemetry health is unreadable")
	}
	status := health["status"]
	if status != nil {
		value, valid := status.(string)
		if !valid || (value != "OK" && value != "DEGRADED") {
			return "", "", errors.New("telemetry health status is invalid")
		}
	}
	if value := health["failure_count"]; value != nil {
		n, err := observationHealthNumber(value)
		if number, ok := value.(json.Number); ok && strings.ContainsAny(string(number), ".eE") {
			err = errors.New("not an integer")
		}
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return "", "", errors.New("failure_count is not a non-negative integer")
		}
	}
	if value := health["last_error_code"]; value != nil {
		if _, ok := value.(string); !ok || !observationSafeText.MatchString(value.(string)) || normalizeObservationID(value, "unknown") != value {
			return "", "", errors.New("last_error_code is not a sanitized identifier")
		}
	}
	for _, key := range []string{"updated_at", "last_projection_write_at"} {
		if value := health[key]; value != nil {
			if err := observationHealthTimestamp(value); err != nil {
				return "", "", fmt.Errorf("%s is not a UTC timestamp", key)
			}
		}
	}
	if lastProjectionWriteAt != nil {
		if err := observationHealthTimestamp(*lastProjectionWriteAt); err != nil {
			return "", "", errors.New("last_projection_write_at is not a UTC timestamp")
		}
		health["last_projection_write_at"] = *lastProjectionWriteAt
	}
	count := 0
	tasksRoot := filepath.Join(root, "tasks")
	entries, err := os.ReadDir(tasksRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("cannot read task aggregates: %w", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(tasksRoot, entry.Name()))
		if readErr != nil {
			return "", "", fmt.Errorf("task aggregate cannot be read: %s", entry.Name())
		}
		aggregate, decodeErr := decodeObservationHealthObject(data)
		if decodeErr != nil || aggregate["task_id"] != strings.TrimSuffix(entry.Name(), ".json") {
			return "", "", fmt.Errorf("task aggregate identity is invalid: %s", entry.Name())
		}
		if _, err := prepareObservationRun(aggregate); err != nil {
			return "", "", err
		}
		count++
	}
	body := "# Telemetry Health\n\n" +
		"This is observational health for local telemetry and projection; it is not an authority for routing or acceptance.\n\n" +
		fmt.Sprintf("- Status: %s\n", observationHealthDisplay(status)) +
		fmt.Sprintf("- Failure count: %s\n", observationHealthDisplay(health["failure_count"])) +
		fmt.Sprintf("- Last error code: %s\n", observationHealthDisplay(health["last_error_code"])) +
		fmt.Sprintf("- Health updated at (UTC): %s\n", observationHealthDisplay(health["updated_at"])) +
		fmt.Sprintf("- Last successful projection write (UTC): %s\n", observationHealthDisplay(health["last_projection_write_at"])) +
		fmt.Sprintf("- Aggregate count: n=%d\n", count) +
		"- Projection mode: local-only, deterministic, no model/network/process access.\n"
	return observationWithMarker("", body, false), path, nil
}

func observationHealthDisplay(value any) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprint(value)
}

func observationHealthNumber(value any) (float64, error) {
	switch n := value.(type) {
	case json.Number:
		return n.Float64()
	case float64:
		return n, nil
	default:
		return 0, errors.New("not numeric")
	}
}

func observationHealthTimestamp(value any) error {
	s, ok := value.(string)
	if !ok || len(s) > 40 || strings.ContainsAny(s, "\r\n") {
		return errors.New("not a UTC timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return errors.New("not a UTC timestamp")
	}
	return nil
}

func decodeObservationHealthObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, errors.New("not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return value, nil
}

type observationRun struct {
	values         map[string]any
	taskID         string
	started, ended *time.Time
}

var observationUsageKeys = []string{
	"codex_call_count", "codex_duration_ms", "codex_usage_known", "gemini_duration_ms", "gemini_usage_known", "deepseek_usage_known",
	"claude_usage_session_id", "claude_usage_captured_at", "claude_5h_start_pct", "claude_5h_end_pct", "claude_5h_delta_pct", "claude_5h_resets_at",
	"claude_7d_start_pct", "claude_7d_end_pct", "claude_7d_resets_at", "claude_context_used_percentage", "claude_context_window_size", "claude_context_total_tokens",
	"claude_context_input_tokens", "claude_context_output_tokens", "claude_cache_creation_input_tokens", "claude_cache_read_input_tokens", "claude_session_estimated_cost_usd",
	"claude_session_total_duration_ms", "claude_usage_known", "claude_rate_limit_delta_scope", "claude_task_usage_source", "claude_task_usage_known",
	"claude_task_usage_complete", "claude_task_input_tokens", "claude_task_output_tokens", "claude_task_cache_creation_input_tokens",
	"claude_task_cache_read_input_tokens", "claude_task_total_observed_tokens", "claude_task_request_count", "claude_task_usage_boundary",
	"claude_task_usage_dedup", "claude_task_usage_unknown_reason", "claude_task_subagent_usage_included",
}

func observationAllUsageKeys() []string {
	keys := append([]string(nil), observationUsageKeys...)
	for _, provider := range []string{"codex", "gemini", "deepseek"} {
		fields := strings.Fields("input_tokens cached_input_tokens output_tokens reasoning_output_tokens total_tokens prompt_tokens completion_tokens cached_tokens cache_read_tokens cache_write_tokens thinking_tokens")
		sort.Strings(fields)
		for _, field := range fields {
			keys = append(keys, provider+"_"+field)
		}
	}
	return keys
}

var observationContextKeys = strings.Fields(`read_count full_read_count targeted_read_count unknown_read_count observed_bytes_total exact_line_read_count exact_line_count_total lower_bound_line_read_count lower_bound_line_count_total oversized_read_count binary_suspected_read_count threshold_200_known_count threshold_200_candidate_count threshold_350_known_count threshold_350_candidate_count threshold_500_known_count threshold_500_candidate_count repeat_count repeat_known pre_codex_read_count post_codex_read_count task_association_known hook_sample_count internal_processing_us_total internal_processing_us_max hook_wall_time_us_total hook_wall_time_us_max unknown_reason`)

var observationRunOptionalBooleans = strings.Fields("first_pass first_pass_success repair repair_attempted repaired accepted blocked")
var observationRunOptionalCounts = []string{"repair_count", "reviewer1_answered_finding_count"}
var observationRunOptionalFindingLists = []string{"reviewer1_answered_finding_refs"}
var observationSafeText = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var observationFindingID = regexp.MustCompile(`^F[0-9a-f]{16}-[0-9]{4}$`)
var observationRunFrontmatterKeys = strings.Fields("type schema_version projection_schema_version task_id repo branch timezone started_at started_at_utc ended_at ended_at_utc status phase route model effort fresh_threads reply_calls routing_enforced validation duration_ms thread_id reviewer1_provider reviewer1_model_requested reviewer1_result reviewer1_skipped_reason served_model_attestation claude_review_mode changed_paths_count routing_protocol_violations thread_continuity_violations concurrent_writer policy_fingerprint first_pass first_pass_success repair repair_attempted repaired accepted blocked repair_count")
var observationRunBaseFrontmatterKeys = strings.Fields("repo branch started_at started_at_utc ended_at ended_at_utc status phase route model effort fresh_threads reply_calls routing_enforced validation duration_ms thread_id reviewer1_provider reviewer1_model_requested reviewer1_result reviewer1_skipped_reason served_model_attestation claude_review_mode changed_paths_count routing_protocol_violations thread_continuity_violations concurrent_writer policy_fingerprint")

func observationResolveDisplayZone() (*time.Location, string) {
	if zone, err := time.LoadLocation("Europe/Istanbul"); err == nil {
		return zone, "Europe/Istanbul"
	}
	return time.FixedZone("UTC+03:00", 3*60*60), "Europe/Istanbul"
}

var observationDisplayZone, observationDisplayTimezone = observationResolveDisplayZone()

func prepareObservationRun(a map[string]any) (observationRun, error) {
	bad := func(name, why string) (observationRun, error) {
		return observationRun{}, fmt.Errorf("%s %s", name, why)
	}
	identifier := func(name string, v any, nullable bool) (any, error) {
		if v == nil && nullable {
			return nil, nil
		}
		s, ok := v.(string)
		if !ok || !observationSafeText.MatchString(s) || normalizeObservationID(s, "unknown") != s {
			return nil, fmt.Errorf("%s is not a sanitized identifier", name)
		}
		return s, nil
	}
	number := func(name string, v any, integer, nullable bool) (any, error) {
		if v == nil && nullable {
			return nil, nil
		}
		var n float64
		switch x := v.(type) {
		case int:
			n = float64(x)
		case int64:
			n = float64(x)
		case float64:
			n = x
		case json.Number:
			parsed, e := x.Float64()
			if e != nil {
				return nil, fmt.Errorf("%s is not numeric", name)
			}
			n = parsed
		default:
			return nil, fmt.Errorf("%s is not numeric", name)
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return nil, fmt.Errorf("%s is not a finite non-negative number", name)
		}
		if integer {
			switch x := v.(type) {
			case int, int64:
			case json.Number:
				if strings.ContainsAny(string(x), ".eE") {
					return nil, fmt.Errorf("%s is not an integer", name)
				}
			default:
				return nil, fmt.Errorf("%s is not an integer", name)
			}
		}
		return v, nil
	}
	boolean := func(name string, v any, nullable bool) (any, error) {
		if v == nil && nullable {
			return nil, nil
		}
		if _, ok := v.(bool); !ok {
			return nil, fmt.Errorf("%s is not boolean", name)
		}
		return v, nil
	}
	v := map[string]any{}
	id, err := identifier("task_id", a["task_id"], false)
	if err != nil {
		return bad("task_id", "is not a sanitized identifier")
	}
	taskID := id.(string)
	copyID := func(key string) error {
		x, e := identifier(key, a[key], true)
		if e == nil {
			v[key] = x
		}
		return e
	}
	for _, key := range []string{"repo", "branch", "model", "thread_id", "reviewer1_provider", "reviewer1_model_requested", "policy_fingerprint"} {
		if err = copyID(key); err != nil {
			return bad(key, "is not a sanitized identifier")
		}
	}
	for _, key := range observationRunBaseFrontmatterKeys {
		if _, ok := v[key]; !ok {
			v[key] = nil
		}
	}
	var started, ended *time.Time
	for _, key := range []string{"started_at", "ended_at"} {
		raw := a[key]
		utcKey := key + "_utc"
		if raw == nil {
			v[key], v[utcKey] = nil, nil
			continue
		}
		s, ok := raw.(string)
		if !ok || len(s) > 40 || strings.ContainsAny(s, "\r\n") {
			return bad(key, "is not a UTC timestamp")
		}
		t, e := time.Parse(time.RFC3339Nano, s)
		if e != nil || !strings.HasSuffix(s, "Z") && !strings.HasSuffix(s, "+00:00") && !strings.HasSuffix(s, "-00:00") {
			return bad(key, "is not a UTC timestamp")
		}
		if key == "started_at" {
			started = &t
		} else {
			ended = &t
		}
		t = t.Truncate(time.Microsecond)
		v[key] = observationPythonLocalTime(t)
		v[utcKey] = s
	}
	setEnum := func(key string, allowed string) error {
		x := a[key]
		if x == nil {
			v[key] = nil
			return nil
		}
		s, ok := x.(string)
		if !ok || !oneOf(s, allowed) {
			return fmt.Errorf("%s is outside its controlled enum", key)
		}
		v[key] = s
		return nil
	}
	for _, enum := range []struct{ key, allowed string }{
		{"status", "UNKNOWN_OR_IN_PROGRESS IN_PROGRESS VERIFIED NOT_VERIFIED INCONCLUSIVE NO_CHANGE"},
		{"phase", "implementation operation"}, {"task_type", "implementation operation plan unknown"},
		{"route", "mechanical subtle normal consequential escalated operation"}, {"effort", "low medium high xhigh"},
		{"validation", "PASS FAIL UNKNOWN"}, {"claude_review_mode", "AUDIT FULL"},
		{"reviewer1_result", "NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE CHANGES_REQUIRED UNCERTAIN REVIEWER1_UNAVAILABLE"},
		{"reviewer1_skipped_reason", "PHASE_OPERATION NO_DELTA BELOW_THRESHOLD REVIEWER_UNAVAILABLE"},
	} {
		key, allowed := enum.key, enum.allowed
		if key == "task_type" {
			if _, exists := a[key]; !exists {
				v[key] = "unknown"
				continue
			}
		}
		if err = setEnum(key, allowed); err != nil {
			return bad(key, "is outside its controlled enum")
		}
	}
	att := a["served_model_attestation"]
	if att != nil && att != "UNAVAILABLE" {
		return bad("served_model_attestation", "is outside its controlled enum")
	}
	v["served_model_attestation"] = att
	for _, key := range []string{"fresh_threads", "reply_calls", "duration_ms", "changed_paths_count", "routing_protocol_violations", "thread_continuity_violations"} {
		x, e := number(key, a[key], key != "duration_ms", true)
		if e != nil {
			return observationRun{}, e
		}
		v[key] = x
	}
	for _, key := range []string{"routing_enforced", "concurrent_writer"} {
		x, e := boolean(key, a[key], true)
		if e != nil {
			return bad(key, "is not boolean")
		}
		v[key] = x
	}
	for _, key := range observationRunOptionalBooleans {
		if x, ok := a[key]; ok {
			z, e := boolean(key, x, false)
			if e != nil {
				return bad(key, "is not boolean")
			}
			v[key] = z
		}
	}
	for _, key := range observationRunOptionalCounts {
		if x, ok := a[key]; ok {
			z, e := number(key, x, true, false)
			if e != nil {
				return observationRun{}, e
			}
			v[key] = z
		}
	}
	for _, key := range observationRunOptionalFindingLists {
		if raw, ok := a[key]; ok {
			if err := validateObservationValue(key, raw); err != nil {
				return bad(key, "is invalid")
			}
			v[key] = raw
		}
	}
	usage, _ := a["usage"].(map[string]any)
	if a["usage"] != nil && usage == nil {
		return bad("usage", "is not an object")
	}
	if usage == nil {
		usage = map[string]any{}
	}
	allUsage := observationAllUsageKeys()
	known := map[string]bool{}
	for _, k := range allUsage {
		known[k] = true
	}
	for _, k := range []string{"_claude_5h_start_session_id", "_claude_5h_start_captured_at", "_claude_5h_start_resets_at", "_claude_5h_end_session_id", "_claude_5h_end_captured_at", "_claude_5h_end_resets_at"} {
		known[k] = true
	}
	for k := range usage {
		if !known[k] {
			return bad("usage", "contains an unregistered field")
		}
	}
	u := map[string]any{}
	for _, k := range allUsage {
		x := usage[k]
		var z any
		var e error
		allowed := map[string]string{"claude_rate_limit_delta_scope": "account_window_observation", "claude_task_usage_source": "desktop_jsonl_observed", "claude_task_usage_boundary": "anchored_pre_tool_to_stop", "claude_task_usage_dedup": "message_id_request_id"}
		choice, isEnum := allowed[k]
		switch {
		case isEnum:
			z = x
		case strings.HasSuffix(k, "_usage_known") || k == "claude_task_usage_complete" || k == "claude_task_subagent_usage_included":
			z, e = boolean(k, x, false)
		case strings.Contains(k, "session_id") || k == "claude_task_usage_unknown_reason":
			z, e = identifier(k, x, false)
		case strings.Contains(k, "captured_at") || strings.Contains(k, "resets_at"):
			z, e = observationUsageTimestamp(k, x)
		default:
			z, e = number(k, x, strings.HasPrefix(k, "claude_task_") && (strings.HasSuffix(k, "_tokens") || strings.HasSuffix(k, "_count")), false)
		}
		if x == nil {
			z = nil
			e = nil
		}
		if isEnum && x != nil && x != choice {
			e = fmt.Errorf("%s is outside its controlled enum", k)
		}
		if e != nil {
			return observationRun{}, e
		}
		u[k] = z
	}
	v["usage"] = u
	v["task_id"] = taskID
	v["timezone"] = observationDisplayTimezone
	v["schema_version"] = 1
	v["projection_schema_version"] = 1
	if acceptance, exists := a["claude_acceptance"]; exists {
		if err := validateObservationAcceptance(acceptance); err != nil {
			return bad("claude_acceptance", "is invalid")
		}
	}
	ctx := a["context_io"]
	if ctx == nil {
		v["context_io"] = nil
	} else {
		m, ok := ctx.(map[string]any)
		if !ok {
			return bad("context_io", "contains an unregistered field")
		}
		c := map[string]any{}
		allow := map[string]bool{}
		for _, k := range observationContextKeys {
			allow[k] = true
		}
		for k := range m {
			if !allow[k] {
				return bad("context_io", "contains an unregistered field")
			}
		}
		for _, k := range observationContextKeys {
			x := m[k]
			if k == "unknown_reason" {
				if x != nil && !oneOf(x, "BINARY_SUSPECTED BOUNDED_SCAN CORRUPT_STATE DIRECTORY INVALID_INPUT JOURNAL_LIMIT MISSING_FILE MISSING_USER_PROMPT OBSERVATION_LIMIT OUTSIDE_ROOT PRIOR_TURN_MISSING_STOP PROTECTED_PATH RESOURCE_LIMIT STALE_STATE STATE_CONTENTION STAT_UNAVAILABLE") {
					return bad("context_io", "unknown_reason is outside its controlled enum")
				}
				c[k] = x
			} else if k == "repeat_known" || k == "task_association_known" {
				z, e := boolean(k, x, true)
				if e != nil {
					return bad(k, "is not boolean")
				}
				c[k] = z
			} else {
				z, e := number(k, x, true, true)
				if e != nil {
					return observationRun{}, e
				}
				c[k] = z
			}
		}
		v["context_io"] = c
	}
	return observationRun{values: v, taskID: taskID, started: started, ended: ended}, nil
}

func validateObservationAcceptance(value any) error {
	if value == nil {
		return fmt.Errorf("invalid")
	}
	m, ok := value.(map[string]any)
	if !ok || len(m) != 4 {
		return fmt.Errorf("invalid")
	}
	if !oneOf(m["outcome"], "accepted rejected unknown") || !validNumber(m["send_backs"], true) {
		return fmt.Errorf("invalid")
	}
	if _, ok := m["applied_as_is"].(bool); !ok {
		return fmt.Errorf("invalid")
	}
	sources, ok := m["sources"].([]any)
	if !ok {
		return fmt.Errorf("invalid")
	}
	for _, item := range sources {
		src, ok := item.(map[string]any)
		if !ok || (len(src) != 5 && len(src) != 7) || !oneOf(src["kind"], "apply reply discard") || !oneOf(src["decision"], "accepted send_back rejected") {
			return fmt.Errorf("invalid")
		}
		if len(src) == 7 {
			if !oneOf(src["review_attribution"], "REVIEWER1_REFERENCED NON_REVIEWER1_OR_UNATTRIBUTED STALE_OR_CROSS_JOB") {
				return fmt.Errorf("invalid")
			}
			refs, ok := src["review_finding_refs"].([]any)
			if !ok && src["review_attribution"] != "REVIEWER1_REFERENCED" && src["review_finding_refs"] == nil {
				ok = true
			}
			if !ok || len(refs) > 20 || (src["review_attribution"] == "REVIEWER1_REFERENCED" && len(refs) == 0) || (src["review_attribution"] == "NON_REVIEWER1_OR_UNATTRIBUTED" && len(refs) != 0) || (src["review_attribution"] == "STALE_OR_CROSS_JOB" && len(refs) == 0) {
				return fmt.Errorf("invalid")
			}
			seen := map[string]bool{}
			for _, raw := range refs {
				ref, ok := raw.(string)
				if !ok || !observationFindingID.MatchString(ref) || seen[ref] {
					return fmt.Errorf("invalid")
				}
				seen[ref] = true
			}
		}
		if id := src["job_id"]; id != nil {
			s, ok := id.(string)
			if !ok || !observationSafeText.MatchString(s) || normalizeObservationID(s, "unknown") != s {
				return fmt.Errorf("invalid")
			}
		}
		if src["task_type"] != nil && !oneOf(src["task_type"], "implementation operation plan unknown") {
			return fmt.Errorf("invalid")
		}
		ts, ok := src["ts_utc"].(string)
		if !ok {
			return fmt.Errorf("invalid")
		}
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		_, offset := parsed.Zone()
		if err != nil || offset != 0 || !(strings.HasSuffix(ts, "Z") || strings.HasSuffix(ts, "+00:00")) {
			return fmt.Errorf("invalid")
		}
	}
	return nil
}

func observationPythonLocalTime(t time.Time) string {
	local := t.In(observationDisplayZone)
	base := local.Format("2006-01-02T15:04:05")
	if local.Nanosecond() != 0 {
		base += fmt.Sprintf(".%06d", local.Nanosecond()/1000)
	}
	return base + local.Format("-07:00")
}
func observationUsageTimestamp(name string, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("%s is not a UTC timestamp", name)
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	_, offset := t.Zone()
	if e != nil || !(strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "+00:00")) || offset != 0 || len(s) > 40 || strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("%s is not a UTC timestamp", name)
	}
	return s, nil
}

func observationRunFrontmatter(p observationRun) string {
	v := p.values
	lines := []string{"---"}
	for _, k := range observationRunFrontmatterKeys {
		x, ok := v[k]
		if !ok {
			switch k {
			case "type":
				x = "orch-run"
			case "started_at_utc":
				x = nil
			case "ended_at_utc":
				x = nil
			default:
				continue
			}
		}
		lines = append(lines, k+": "+observationYAMLScalar(x))
		if k == "served_model_attestation" {
			for _, optional := range []string{"reviewer1_answered_finding_count", "reviewer1_answered_finding_refs"} {
				if x, ok := v[optional]; ok {
					lines = append(lines, optional+": "+observationYAMLScalar(x))
				}
			}
		}
	}
	lines = append(lines, "usage:")
	u := v["usage"].(map[string]any)
	for _, k := range observationAllUsageKeys() {
		lines = append(lines, "  "+k+": "+observationYAMLScalar(u[k]))
	}
	if v["context_io"] != nil {
		lines = append(lines, "context_io:")
		for _, k := range observationContextKeys {
			lines = append(lines, "  "+k+": "+observationYAMLScalar(v["context_io"].(map[string]any)[k]))
		}
	}
	lines = append(lines, "---", "")
	return strings.Join(lines, "\n")
}
func observationYAMLScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		b, _ := json.Marshal(x)
		s := string(b)
		for _, pair := range [][2]string{{"\\u003c", "<"}, {"\\u003e", ">"}, {"\\u0026", "&"}, {"\\u2028", " "}, {"\\u2029", " "}} {
			s = strings.ReplaceAll(s, pair[0], pair[1])
		}
		return s
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return observationPythonFloat(x)
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return string(x)
		}
		f, err := x.Float64()
		if err != nil {
			return string(x)
		}
		return observationPythonFloat(f)
	default:
		return fmt.Sprint(x)
	}
}

func observationPythonFloat(value float64) string {
	if math.Trunc(value) == value && math.Abs(value) < 1e16 {
		return strconv.FormatFloat(value, 'f', 1, 64)
	}
	abs := math.Abs(value)
	if value != 0 && (abs < 1e-4 || abs >= 1e16) {
		s := strconv.FormatFloat(value, 'e', -1, 64)
		i := strings.LastIndexByte(s, 'e')
		mantissa, exp := s[:i], s[i+1:]
		sign := "+"
		if strings.HasPrefix(exp, "-") {
			sign = "-"
			exp = strings.TrimPrefix(exp, "-")
		} else {
			exp = strings.TrimPrefix(exp, "+")
		}
		if len(exp) < 2 {
			exp = "0" + exp
		}
		return mantissa + "e" + sign + exp
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}
func observationRunBody(p observationRun) string {
	v := p.values
	disp := func(x any) string {
		if x == nil {
			return "unknown"
		}
		if b, ok := x.(bool); ok {
			if b {
				return "True"
			}
			return "False"
		}
		return fmt.Sprint(x)
	}
	duration := "unknown"
	if x := v["duration_ms"]; x != nil {
		var ms float64
		switch n := x.(type) {
		case float64:
			ms = n
		case int:
			ms = float64(n)
		case int64:
			ms = float64(n)
		case json.Number:
			ms, _ = n.Float64()
		}
		seconds := ms / 1000
		if seconds < 60 {
			duration = fmt.Sprintf("%.1fs", seconds)
		} else {
			duration = fmt.Sprintf("%dm %.1fs", int(seconds/60), math.Mod(seconds, 60))
		}
	}
	review := disp(v["reviewer1_result"])
	if v["reviewer1_skipped_reason"] != nil {
		review = "SKIPPED (" + fmt.Sprint(v["reviewer1_skipped_reason"]) + ")"
	}
	return fmt.Sprintf("# Run: %s\n\nFlow: %s / %s (%s), phase=%s; fresh_threads=%s, reply_calls=%s\nDuration: %s\nValidation: %s\nReview: %s\n\n## Notes\n\n- Status: %s\n- Concurrent writer: %s\n- Projection source: telemetry aggregate only; local timezone: %s.\n", p.taskID, disp(v["route"]), disp(v["model"]), disp(v["effort"]), disp(v["phase"]), disp(v["fresh_threads"]), disp(v["reply_calls"]), duration, disp(v["validation"]), review, disp(v["status"]), disp(v["concurrent_writer"]), v["timezone"])
}

func observationGeneratedContentIsIntact(content string) bool {
	match := observationMarker.FindStringSubmatchIndex(content)
	if match == nil {
		return false
	}
	marker := content[match[0]:match[1]]
	digest := ""
	if match[2] >= 0 {
		digest = content[match[2]:match[3]]
	} else {
		digest = content[match[4]:match[5]]
	}
	without := content[:match[0]] + content[match[1]:]
	actual := sha256.Sum256([]byte(without))
	return hex.EncodeToString(actual[:]) == digest && marker != ""
}

func observationMayReplaceExisting(exists bool, content string) bool {
	return !exists || observationGeneratedContentIsIntact(content)
}

// observationAggregateDefaults mirrors telemetry._new_aggregate's routing
// fields. Existing values are overlaid after these defaults by the task fold.
func observationAggregateDefaults(taskID, projectKey any) map[string]any {
	usage := map[string]any{
		"codex_call_count": 0, "codex_duration_ms": nil, "codex_usage_known": false,
		"gemini_duration_ms": nil, "gemini_usage_known": false, "deepseek_usage_known": false,
		"claude_5h_start_pct": nil, "claude_5h_end_pct": nil, "claude_5h_delta_pct": nil,
		"claude_7d_start_pct": nil, "claude_7d_end_pct": nil, "claude_context_used_percentage": nil,
		"claude_session_estimated_cost_usd": nil, "claude_usage_known": false,
		"claude_rate_limit_delta_scope": "account_window_observation",
		"claude_task_usage_source":      nil, "claude_task_usage_known": false, "claude_task_usage_complete": false,
		"claude_task_input_tokens": nil, "claude_task_output_tokens": nil,
		"claude_task_cache_creation_input_tokens": nil, "claude_task_cache_read_input_tokens": nil,
		"claude_task_total_observed_tokens": nil, "claude_task_request_count": nil,
		"claude_task_usage_boundary": nil, "claude_task_usage_dedup": nil,
		"claude_task_usage_unknown_reason": nil, "claude_task_subagent_usage_included": false,
	}
	for _, provider := range []string{"codex", "gemini", "deepseek"} {
		for _, field := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cached_tokens", "cache_read_tokens", "cache_write_tokens", "thinking_tokens"} {
			usage[provider+"_"+field] = nil
		}
	}
	for _, prefix := range []string{"_claude_5h_start_", "_claude_5h_end_"} {
		for _, suffix := range []string{"session_id", "captured_at", "resets_at"} {
			usage[prefix+suffix] = nil
		}
	}
	return map[string]any{
		"task_id": taskID, "project_key": projectKey, "repo": nil, "branch": nil,
		"status": "UNKNOWN_OR_IN_PROGRESS", "started_at": nil, "ended_at": nil, "duration_ms": nil,
		"phase": nil, "task_type": "unknown", "task_type_source": "unknown", "has_code_map": nil,
		"selected_agent": nil, "selected_provider": nil, "selected_model": nil,
		"served_agent": nil, "served_provider": nil, "served_model": nil, "served_route": nil, "served_effort": nil,
		"claude_decision":   nil,
		"claude_acceptance": map[string]any{"outcome": "unknown", "send_backs": 0, "applied_as_is": false, "sources": []any{}},
		"route":             nil, "model": nil, "effort": nil, "validation": nil, "thread_id": nil,
		"fresh_threads": 0, "reply_calls": 0, "routing_enforced": nil,
		"reviewer1_provider": nil, "reviewer1_model_requested": nil, "served_model_attestation": "UNAVAILABLE",
		"reviewer1_result": nil, "reviewer1_initial_finding_count": nil, "reviewer1_skipped_reason": nil,
		"claude_review_mode": nil, "changed_paths_count": nil, "routing_protocol_violations": 0,
		"thread_continuity_violations": 0, "concurrent_writer": false,
		"policy_fingerprint": nil,
		"policy_hashes":      map[string]any{"skill_sha256": nil, "config_sha256": nil, "hook_sha256": nil},
		"usage":              usage, "context_io": nil,
	}
}

// foldObservationAggregate applies the routing-related Python task fold in
// event order and returns whether terminal Claude decisions conflict.
func foldObservationAggregate(existing map[string]any, taskID, projectKey any, events []map[string]any, incremental bool) (map[string]any, bool) {
	defaults := observationAggregateDefaults(taskID, projectKey)
	aggregate := defaults
	if hashes, hasHashes := existing["policy_hashes"]; hasHashes {
		if fingerprint, hasFingerprint := existing["policy_fingerprint"]; hasFingerprint {
			aggregate["policy_hashes"], aggregate["policy_fingerprint"] = hashes, fingerprint
		}
	}
	usage := defaults["usage"].(map[string]any)
	if incremental {
		for key, value := range existing {
			aggregate[key] = value
		}
		if saved, ok := existing["usage"].(map[string]any); ok {
			for key, value := range saved {
				usage[key] = value
			}
		}
	}
	aggregate["usage"] = usage
	ordered := append([]map[string]any(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := fmt.Sprint(ordered[i]["ts_utc"]), fmt.Sprint(ordered[j]["ts_utc"])
		if left != right {
			return left < right
		}
		return fmt.Sprint(ordered[i]["event_id"]) < fmt.Sprint(ordered[j]["event_id"])
	})
	conflict := false
	for _, event := range ordered {
		payload, _ := event["payload"].(map[string]any)
		for _, field := range []string{"repo", "branch"} {
			if aggregate[field] == nil && event[field] != nil {
				aggregate[field] = event[field]
			}
		}
		switch event["event_type"] {
		case "TASK_ROUTE_SELECTED":
			for _, field := range []string{"phase", "route", "model", "effort", "routing_enforced"} {
				if value, ok := payload[field]; ok {
					aggregate[field] = value
				}
			}
			if value, ok := payload["task_type"]; ok {
				aggregate["task_type"], aggregate["task_type_source"] = value, "route"
			}
			for source, target := range map[string]string{"agent": "selected_agent", "provider": "selected_provider", "model": "selected_model"} {
				if value, ok := payload[source]; ok {
					aggregate[target] = value
				}
			}
		case "IMPLEMENTER_STARTED":
			aggregate["status"] = "IN_PROGRESS"
			if aggregate["started_at"] == nil || aggregate["started_at"] == "" {
				aggregate["started_at"] = event["ts_utc"]
			}
			if payload["fresh_thread"] == true {
				aggregate["fresh_threads"] = numberOrZero(aggregate["fresh_threads"]) + 1
			}
			if payload["reply_call"] == true {
				aggregate["reply_calls"] = numberOrZero(aggregate["reply_calls"]) + 1
			}
			usage["codex_call_count"] = numberOrZero(usage["codex_call_count"]) + 1
			foldClaudeUsage(usage, payload, "start")
		case "IMPLEMENTER_FINISHED", "IMPLEMENTER_FAILED":
			aggregate["ended_at"] = event["ts_utc"]
			aggregate["duration_ms"] = addObservationNumber(aggregate["duration_ms"], payload["duration_ms"])
			usage["codex_duration_ms"] = addObservationNumber(usage["codex_duration_ms"], payload["duration_ms"])
			for _, field := range []string{"changed_paths_count", "thread_id", "claude_review_mode", "routing_enforced", "served_agent", "served_provider", "served_model", "served_route", "served_effort"} {
				if value, ok := payload[field]; ok {
					aggregate[field] = value
				}
			}
			if value, ok := payload["has_code_map"]; ok {
				aggregate["has_code_map"] = value
			}
			if aggregate["validation"] == nil && payload["validation"] != nil {
				aggregate["validation"] = payload["validation"]
			}
			known := payload["codex_usage_known"] == true
			if numberOrZero(usage["codex_call_count"]) <= 1 {
				usage["codex_usage_known"] = known
			} else {
				usage["codex_usage_known"] = usage["codex_usage_known"] == true && known
			}
			for _, field := range observationTokenFields {
				key := "codex_" + field
				if value, ok := payload[key]; ok {
					usage[key] = addObservationNumber(usage[key], value)
				}
			}
			foldClaudeUsage(usage, payload, "end")
		case "REVIEWER1_STARTED", "REVIEWER1_FINISHED", "REVIEWER1_FAILED":
			if event["event_type"] == "REVIEWER1_STARTED" {
				aggregate["reviewer1_result"], aggregate["reviewer1_initial_finding_count"], aggregate["reviewer1_skipped_reason"] = nil, nil, nil
				delete(aggregate, "reviewer1_finding_refs")
				delete(aggregate, "reviewer1_job_id")
				delete(aggregate, "reviewer1_answered_finding_refs")
				delete(aggregate, "reviewer1_answered_finding_count")
			} else {
				aggregate["reviewer1_skipped_reason"] = nil
			}
			for _, field := range []string{"reviewer1_provider", "reviewer1_model_requested", "served_model_attestation", "reviewer1_result", "reviewer1_initial_finding_count", "reviewer1_finding_refs", "reviewer1_job_id"} {
				if value, ok := payload[field]; ok {
					aggregate[field] = value
				}
			}
			if event["event_type"] != "REVIEWER1_STARTED" {
				usage["gemini_duration_ms"] = addObservationNumber(usage["gemini_duration_ms"], payload["duration_ms"])
				if payload["gemini_usage_known"] == true {
					usage["gemini_usage_known"] = true
				}
				for _, field := range observationTokenFields {
					key := "gemini_" + field
					if value, ok := payload[key]; ok {
						usage[key] = addObservationNumber(usage[key], value)
					}
				}
			}
		case "REVIEWER1_SKIPPED":
			aggregate["reviewer1_provider"], aggregate["reviewer1_result"], aggregate["reviewer1_initial_finding_count"] = nil, nil, nil
			delete(aggregate, "reviewer1_finding_refs")
			delete(aggregate, "reviewer1_job_id")
			delete(aggregate, "reviewer1_answered_finding_refs")
			delete(aggregate, "reviewer1_answered_finding_count")
			aggregate["reviewer1_skipped_reason"] = payload["reviewer1_skipped_reason"]
			aggregate["reviewer1_model_requested"] = payload["reviewer1_model_requested"]
		case "ROUTING_PROTOCOL_VIOLATION":
			aggregate["routing_protocol_violations"] = numberOrZero(aggregate["routing_protocol_violations"]) + 1
		case "THREAD_CONTINUITY_VIOLATION":
			aggregate["thread_continuity_violations"] = numberOrZero(aggregate["thread_continuity_violations"]) + 1
		case "UNKNOWN_CONCURRENT_WRITER":
			aggregate["concurrent_writer"] = true
		case "CLAUDE_DECISION_RECORDED":
			acceptance, _ := aggregate["claude_acceptance"].(map[string]any)
			source, _ := payload["source"].(map[string]any)
			sources, _ := acceptance["sources"].([]any)
			duplicate := false
			for _, raw := range sources {
				if item, ok := raw.(map[string]any); ok && item["kind"] == source["kind"] && item["job_id"] == source["job_id"] && item["review_attribution"] == payload["review_attribution"] &&
					(item["review_finding_refs"] == nil && payload["review_finding_refs"] == nil || postSameStrings(item["review_finding_refs"], payload["review_finding_refs"])) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			terminalsBefore := map[string]bool{}
			for _, raw := range sources {
				if item, ok := raw.(map[string]any); ok {
					decision := fmt.Sprint(item["decision"])
					if decision == "accepted" || decision == "rejected" {
						terminalsBefore[decision] = true
					}
				}
			}
			decision := payload["decision"]
			entry := map[string]any{"kind": source["kind"], "job_id": source["job_id"], "decision": decision, "task_type": payload["task_type"], "ts_utc": event["ts_utc"]}
			if attribution, ok := payload["review_attribution"].(string); ok {
				refs := payload["review_finding_refs"]
				if refs == nil {
					refs = []any{}
				}
				if attribution == "REVIEWER1_REFERENCED" && (source["job_id"] != aggregate["reviewer1_job_id"] || !reviewFindingRefsMatch(refs, aggregate["reviewer1_finding_refs"])) {
					attribution = "STALE_OR_CROSS_JOB"
				} else if attribution == "REVIEWER1_REFERENCED" {
					aggregate["reviewer1_answered_finding_refs"] = mergeFindingReferences(aggregate["reviewer1_answered_finding_refs"], refs)
					answered, _ := aggregate["reviewer1_answered_finding_refs"].([]any)
					aggregate["reviewer1_answered_finding_count"] = len(answered)
				}
				entry["review_attribution"], entry["review_finding_refs"] = attribution, refs
			}
			sources = append(sources, entry)
			acceptance["sources"] = sources
			aggregate["claude_decision"] = map[string]any{"decision": decision, "task_type": payload["task_type"], "source": source, "ts_utc": event["ts_utc"]}
			if aggregate["task_type"] == "unknown" && payload["task_type"] != "unknown" {
				aggregate["task_type"], aggregate["task_type_source"] = payload["task_type"], "decision"
			}
			if decision == "send_back" {
				acceptance["send_backs"] = numberOrZero(acceptance["send_backs"]) + 1
			}
			terminals := map[string]bool{}
			for _, raw := range sources {
				if item, ok := raw.(map[string]any); ok {
					d := fmt.Sprint(item["decision"])
					if d == "accepted" || d == "rejected" {
						terminals[d] = true
					}
				}
			}
			outcome := "unknown"
			if len(terminals) == 1 {
				for d := range terminals {
					outcome = d
				}
			}
			acceptance["outcome"] = outcome
			acceptance["applied_as_is"] = outcome == "accepted" && numberOrZero(acceptance["send_backs"]) == 0
			conflict = conflict || len(terminals) > 1 && len(terminalsBefore) <= 1
		case "CLAUDE_TASK_USAGE_RECORDED":
			foldClaudeTaskUsage(usage, payload)
		case "CONTEXT_IO_TURN_RECORDED":
			copy := map[string]any{}
			for key, value := range payload {
				copy[key] = value
			}
			aggregate["context_io"] = copy
		}
	}
	return aggregate, conflict
}

var observationTokenFields = strings.Fields("input_tokens cached_input_tokens output_tokens reasoning_output_tokens total_tokens prompt_tokens completion_tokens cached_tokens cache_read_tokens cache_write_tokens thinking_tokens")

func addObservationNumber(current, value any) any {
	var n float64
	switch x := value.(type) {
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case float64:
		n = x
	default:
		return current
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return current
	}
	if current == nil {
		return value
	}
	switch x := current.(type) {
	case int:
		return float64(x) + n
	case int64:
		return float64(x) + n
	case float64:
		return x + n
	}
	return current
}

func foldClaudeUsage(usage, payload map[string]any, endpoint string) {
	if payload["claude_usage_known"] == true {
		usage["claude_usage_known"] = true
	}
	if payload["claude_rate_limit_delta_scope"] == "account_window_observation" {
		usage["claude_rate_limit_delta_scope"] = "account_window_observation"
	}
	for _, field := range []string{"claude_context_used_percentage", "claude_session_estimated_cost_usd"} {
		if n := observationClaudeNumber(payload[field]); n != nil {
			usage[field] = n
		}
	}
	endpointField := "claude_5h_" + endpoint + "_pct"
	endpointValue := observationClaudeNumber(payload[endpointField])
	if endpoint == "start" {
		if endpointValue != nil && usage["claude_5h_start_pct"] == nil {
			usage["claude_5h_start_pct"] = endpointValue
			setClaudeUsageEndpoint(usage, payload, "start")
		}
	} else if endpointValue != nil {
		usage["claude_5h_end_pct"] = endpointValue
		setClaudeUsageEndpoint(usage, payload, "end")
	}
	if n := observationClaudeNumber(payload["claude_7d_"+endpoint+"_pct"]); n != nil {
		usage["claude_7d_"+endpoint+"_pct"] = n
	}
	start, end := observationClaudeNumber(usage["claude_5h_start_pct"]), observationClaudeNumber(usage["claude_5h_end_pct"])
	if start == nil || end == nil || usage["_claude_5h_start_session_id"] == nil || usage["_claude_5h_start_session_id"] != usage["_claude_5h_end_session_id"] || claudeResetCrossed(usage) {
		usage["claude_5h_delta_pct"] = nil
		return
	}
	if startAt, ok1 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_start_captured_at"])); ok1 {
		if endAt, ok2 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_end_captured_at"])); ok2 && endAt.Before(startAt) {
			usage["claude_5h_delta_pct"] = nil
			return
		}
	}
	usage["claude_5h_delta_pct"] = end.(float64) - start.(float64)
}

func setClaudeUsageEndpoint(usage, payload map[string]any, endpoint string) {
	for suffix, field := range map[string]string{"session_id": "claude_usage_session_id", "captured_at": "claude_usage_captured_at", "resets_at": "claude_5h_resets_at"} {
		usage["_claude_5h_"+endpoint+"_"+suffix] = payload[field]
	}
}

func observationClaudeNumber(value any) any {
	switch n := value.(type) {
	case int:
		if n >= 0 {
			return float64(n)
		}
	case int64:
		if n >= 0 {
			return float64(n)
		}
	case float64:
		if n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n
		}
	}
	return nil
}

func claudeResetCrossed(usage map[string]any) bool {
	startReset, ok1 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_start_resets_at"]))
	endReset, ok2 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_end_resets_at"]))
	if ok1 && ok2 && !startReset.Equal(endReset) {
		return true
	}
	startAt, ok3 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_start_captured_at"]))
	endAt, ok4 := parseClaudeUsageTimestamp(asString(usage["_claude_5h_end_captured_at"]))
	return ok1 && ok2 && ok3 && ok4 && !startAt.After(endAt) && !startReset.Before(startAt) && !startReset.After(endAt)
}

func foldClaudeTaskUsage(usage, payload map[string]any) {
	terminal := fields("OVERLAPPING_TASK_BOUNDARY CONTRADICTORY_USAGE_RECORD TRANSCRIPT_IDENTITY_MISMATCH TRANSCRIPT_ROTATED_OR_TRUNCATED TRANSCRIPT_UNRESOLVED_TAIL BOUNDS_EXCEEDED INVALID_USAGE_RECORD STALE_ACTIVE_TASK ANCHOR_NOT_FOUND TRANSCRIPT_UNAVAILABLE SESSION_MISMATCH WRONG_EXTENSION TRANSCRIPT_PATH_ESCAPE NO_USAGE_RECORDS ACTIVE_TASK_STATE_CONTENTION")
	currentReason := asString(usage["claude_task_usage_unknown_reason"])
	if terminal[currentReason] {
		return
	}
	newReason := payload["claude_task_usage_unknown_reason"]
	known := payload["claude_task_usage_known"] == true && !terminal[asString(newReason)]
	fieldsToClear := []string{"claude_task_input_tokens", "claude_task_output_tokens", "claude_task_cache_creation_input_tokens", "claude_task_cache_read_input_tokens", "claude_task_total_observed_tokens", "claude_task_request_count"}
	if terminal[asString(newReason)] || !known {
		usage["claude_task_usage_known"], usage["claude_task_usage_complete"] = false, false
		usage["claude_task_usage_unknown_reason"] = newReason
		usage["claude_task_usage_source"], usage["claude_task_usage_boundary"], usage["claude_task_usage_dedup"] = nil, nil, nil
		for _, field := range fieldsToClear {
			usage[field] = nil
		}
		usage["claude_task_subagent_usage_included"] = false
		return
	}
	usage["claude_task_usage_known"] = true
	usage["claude_task_usage_complete"] = payload["claude_task_usage_complete"] == true
	usage["claude_task_usage_unknown_reason"] = nil
	for _, field := range []string{"claude_task_usage_source", "claude_task_usage_boundary", "claude_task_usage_dedup"} {
		usage[field] = payload[field]
	}
	usage["claude_task_subagent_usage_included"] = payload["claude_task_subagent_usage_included"] == true
	for _, field := range fieldsToClear {
		if n, ok := payload[field].(float64); ok && n >= 0 && math.Trunc(n) == n {
			usage[field] = n
		}
	}
}

func numberOrZero(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func reviewFindingRefsMatch(value, current any) bool {
	refs, ok := value.([]any)
	if !ok || len(refs) == 0 {
		return false
	}
	available := map[string]bool{}
	switch items := current.(type) {
	case []any:
		for _, item := range items {
			if ref, ok := item.(string); ok {
				available[ref] = true
			}
		}
	case []string:
		for _, ref := range items {
			available[ref] = true
		}
	}
	for _, item := range refs {
		ref, ok := item.(string)
		if !ok || !available[ref] {
			return false
		}
	}
	return true
}

func mergeFindingReferences(existing, added any) []any {
	result := []any{}
	seen := map[string]bool{}
	for _, values := range []any{existing, added} {
		items, _ := values.([]any)
		for _, item := range items {
			ref, ok := item.(string)
			if ok && !seen[ref] {
				seen[ref] = true
				result = append(result, ref)
			}
		}
	}
	return result
}

func fields(names string) map[string]bool {
	out := map[string]bool{}
	for _, name := range strings.Fields(names) {
		out[name] = true
	}
	return out
}

var (
	callObservationFields  = fields("phase route model model_source effort routing_enforced fresh_thread reply_call reason reason_source previous_route new_route route_adjustment_reason eta_minutes worker_prompt_chars scope_path_count task_type suspected_route_mismatch agent provider")
	startObservationFields = fields("implementer claude_usage_session_id claude_usage_captured_at claude_5h_start_pct claude_5h_end_pct claude_5h_resets_at claude_7d_start_pct claude_7d_end_pct claude_7d_resets_at claude_context_used_percentage claude_context_window_size claude_context_total_tokens claude_context_input_tokens claude_context_output_tokens claude_cache_creation_input_tokens claude_cache_read_input_tokens claude_session_estimated_cost_usd claude_session_total_duration_ms claude_usage_known claude_rate_limit_delta_scope")
	observationFields      = map[string]map[string]bool{"TASK_ROUTE_SELECTED": callObservationFields, "IMPLEMENTER_STARTED": mergeFields(callObservationFields, startObservationFields), "IMPLEMENTER_FAILED": mergeFields(fields("failure_code duration_ms eta_minutes reason reason_source previous_route new_route longest_silent_gap_seconds"), claudeUsagePayloadFields), "REVIEWER1_STARTED": fields("reviewer1_provider reviewer1_model_requested"), "REVIEWER1_FINISHED": reviewerFinishedObservationFields, "REVIEWER1_FAILED": reviewerFailedObservationFields, "REVIEWER1_SKIPPED": fields("reviewer1_skipped_reason reviewer1_model_requested served_model_attestation"), "SCOPE_CONFLICT_OUTCOME": fields("action job_id queue_wait_ms"), "ROUTING_PROTOCOL_VIOLATION": fields("violation_code"), "THREAD_CONTINUITY_VIOLATION": fields("violation_code"), "CODEX_RESULT_UNFETCHED": fields("route code"), "CLAUDE_DECISION_RECORDED": fields("decision task_type source reviewer1_valid_count reviewer1_false_count review_attribution review_finding_refs"), "READINESS_CHECK_OUTCOME": fields("verdict agent duration_ms"), "PLAN_REDIRECT": fields("route non_test_scope_count"), "MAINTENANCE_REPORT_FINISHED": fields("report_id report_type outcome reason_code elapsed_seconds quota_change_active_window_pct findings_high_count findings_medium_count findings_low_count line_coverage_pct")}
	observationID          = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	observationUUID        = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	unsafeObservationID    = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	observationProjectKey  = regexp.MustCompile(`^(?:[0-9a-f]{16}|unresolved)$`)
	modelValue             = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)
)

// persistObservation mirrors telemetry.emit's best-effort append contract.
// On lock contention the complete event is atomically spooled and is not
// written to the task index until a later recovery pass merges it.
func persistObservation(root string, event map[string]any) (string, error) {
	eventID, ok := event["event_id"].(string)
	if !ok || !observationID.MatchString(eventID) {
		return "TELEMETRY_EMIT_FAILED", fmt.Errorf("invalid telemetry event id")
	}
	encoded, err := canonicalJSON(event)
	if err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	events := filepath.Join(root, "events")
	if err = os.MkdirAll(events, 0700); err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	lockPath := filepath.Join(events, ".append.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	defer lock.Close()
	if err = contextio.TryLock(lock); err != nil {
		spool := filepath.Join(events, "spool", eventID+".json")
		if err = atomicObservationJSON(spool, encoded); err != nil {
			return "TELEMETRY_EMIT_FAILED", err
		}
		return "", nil
	}
	defer contextio.Unlock(lock)
	if err = mergeObservationSpool(root); err != nil {
		return "TELEMETRY_SPOOL_MERGE_FAILED", err
	}
	month := ""
	if timestamp, valid := event["ts_utc"].(string); valid && len(timestamp) >= 7 {
		month = timestamp[:7]
	}
	session, _ := event["session_id"].(string)
	if session == "" {
		session = "unknown"
	}
	path := filepath.Join(events, month, "session_"+session+".jsonl")
	if err = appendObservationRecord(path, encoded); err != nil {
		return "TELEMETRY_EMIT_FAILED", err
	}
	if task, ok := event["task_id"].(string); ok {
		if err = appendObservationRecord(filepath.Join(events, "by_task", task+".jsonl"), encoded); err != nil {
			return "TELEMETRY_EMIT_FAILED", err
		}
	}
	return "", nil
}

const observationSpoolMergeLimit = 16

// mergeObservationSpool mirrors telemetry._merge_spool. The marker is created
// before writes, as in Python; on recovery each destination is checked by
// event ID so a crash between either append resumes without duplicates.
func mergeObservationSpool(root string) error {
	spool := filepath.Join(root, "events", "spool")
	entries, err := os.ReadDir(spool)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	merged := filepath.Join(spool, ".merged")
	if err = os.MkdirAll(merged, 0700); err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if count == observationSpoolMergeLimit {
			break
		}
		count++
		path := filepath.Join(spool, entry.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			recordObservationHealth(root, "DEGRADED", "MALFORMED_SPOOL_EVENT")
			_ = os.Remove(path)
			continue
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			recordObservationHealth(root, "DEGRADED", "MALFORMED_SPOOL_EVENT")
			_ = os.Remove(path)
			continue
		}
		if !validObservationSpoolEvent(event) {
			recordObservationHealth(root, "DEGRADED", "MALFORMED_SPOOL_EVENT")
			_ = os.Remove(path)
			continue
		}
		eventID := event["event_id"].(string)
		marker := filepath.Join(merged, eventID)
		if _, statErr := os.Stat(marker); os.IsNotExist(statErr) {
			file, createErr := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if createErr != nil && !os.IsExist(createErr) {
				return createErr
			}
			if createErr == nil {
				if closeErr := file.Close(); closeErr != nil {
					return closeErr
				}
			}
		} else if statErr != nil {
			return statErr
		}
		line := append(append([]byte(nil), raw...), '\n')
		ts := event["ts_utc"].(string)
		session, _ := event["session_id"].(string)
		if session == "" {
			session = "unknown"
		}
		canonical := filepath.Join(root, "events", ts[:7], "session_"+session+".jsonl")
		if err = appendObservationOnce(canonical, line, eventID); err != nil {
			return err
		}
		if task, ok := event["task_id"].(string); ok {
			if err = appendObservationOnce(filepath.Join(root, "events", "by_task", task+".jsonl"), line, eventID); err != nil {
				return err
			}
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func validObservationSpoolEvent(event map[string]any) bool {
	keys := []string{"schema_version", "event_id", "ts_utc", "event_type", "task_id", "project_key", "session_id", "repo", "branch", "agent_id", "agent_type", "payload"}
	if len(event) != len(keys) || !observationUUID.MatchString(asString(event["event_id"])) || event["schema_version"] != float64(observationSchemaVersion) {
		return false
	}
	for _, key := range keys {
		if _, ok := event[key]; !ok {
			return false
		}
	}
	ts, ok := event["ts_utc"].(string)
	if !ok || len(ts) > 40 {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return false
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return false
	}
	task := event["task_id"]
	envelope := map[string]any{}
	for _, name := range []string{"project_key", "session_id", "repo", "branch", "agent_id", "agent_type"} {
		envelope[name] = event[name]
	}
	payload, ok := event["payload"].(map[string]any)
	if !ok {
		return false
	}
	built, err := buildObservation(asString(event["event_type"]), envelope, payload, task)
	if err != nil || !reflect.DeepEqual(built["payload"], payload) {
		return false
	}
	for _, name := range []string{"task_id", "project_key", "session_id", "repo", "branch", "agent_id", "agent_type"} {
		if !reflect.DeepEqual(built[name], event[name]) {
			return false
		}
	}
	return true
}

func asString(value any) string {
	s, _ := value.(string)
	return s
}

// appendObservationOnce repairs a torn final line and de-duplicates by event
// identity before appending. A valid prior record also covers crash recovery
// when its marker was persisted before the spool file was removed.
func appendObservationOnce(path string, line []byte, eventID string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lastLine := strings.LastIndexByte(string(data), '\n') + 1
			tail := data[lastLine:]
			var prior map[string]any
			if json.Unmarshal(tail, &prior) == nil && prior["event_id"] == eventID {
				f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
				if openErr != nil {
					return openErr
				}
				if _, openErr = f.Write([]byte{'\n'}); openErr == nil {
					openErr = f.Sync()
				}
				closeErr := f.Close()
				if openErr != nil {
					return openErr
				}
				if closeErr != nil {
					return closeErr
				}
				return nil
			}
			f, openErr := os.OpenFile(path, os.O_WRONLY, 0600)
			if openErr != nil {
				return openErr
			}
			if openErr = f.Truncate(int64(lastLine)); openErr == nil {
				openErr = f.Sync()
			}
			closeErr := f.Close()
			if openErr != nil {
				return openErr
			}
			if closeErr != nil {
				return closeErr
			}
			data = data[:lastLine]
		}
		for _, record := range strings.Split(string(data), "\n") {
			var prior map[string]any
			if record != "" && json.Unmarshal([]byte(record), &prior) == nil && prior["event_id"] == eventID {
				return nil
			}
		}
	}
	return appendObservationRecord(path, line[:len(line)-1])
}

func appendObservationRecord(path string, encoded []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	line := append(append([]byte(nil), encoded...), '\n')
	_, err = f.Write(line)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func atomicObservationJSON(path string, encoded []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(encoded); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}

var observationProjectionNow = time.Now

func atomicObservationNote(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if data, err := os.ReadFile(path); err == nil && !observationGeneratedContentIsIntact(string(data)) {
		return fmt.Errorf("refusing to overwrite non-projection note")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = io.WriteString(f, content); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}

func recordObservationProjectionFailure(root string, now time.Time) {
	defer func() { _ = recover() }()
	path := filepath.Join(root, "telemetry", "telemetry_health.json")
	data, err := os.ReadFile(path)
	health := map[string]any{"schema_version": 1, "status": "OK", "failure_count": 0, "last_error_code": nil}
	if err == nil {
		var saved map[string]any
		if json.Unmarshal(data, &saved) == nil && saved != nil {
			health = saved
		}
	}
	count, ok := health["failure_count"].(float64)
	if !ok {
		if n, yes := health["failure_count"].(int); yes {
			count, ok = float64(n), true
		} else if value, yes := health["failure_count"].(bool); yes {
			if value {
				count = 1
			}
			ok = true
		}
	}
	if !ok || count < 0 || math.Trunc(count) != count {
		count = 0
	}
	health["schema_version"], health["status"], health["failure_count"], health["last_error_code"] = 1, "DEGRADED", count+1, "TELEMETRY_PROJECTION_FAILED"
	health["updated_at"] = now.UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(health)
	if err == nil {
		_ = atomicObservationJSON(path, encoded)
	}
}

func projectObservationTask(root, taskID, projectKey, configPath string, envVault *string) {
	state, vault := filepath.Dir(root), ""
	defer func() {
		if recover() != nil {
			recordObservationProjectionFailure(state, observationProjectionNow())
			if vault != "" {
				writeObservationHealthFallback(state, vault)
			}
		}
	}()
	config, _ := os.ReadFile(configPath)
	enabled, resolvedVault := observationProjectionSettings(config, envVault, nil)
	if !enabled || resolvedVault == "" {
		return
	}
	vault = resolvedVault
	projectState := filepath.Join(state, "projects", projectKey)
	aggregatePath := filepath.Join(projectState, "telemetry", "tasks", taskID+".json")
	var aggregate map[string]any
	if data, err := os.ReadFile(aggregatePath); err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		_ = decoder.Decode(&aggregate)
	}
	content, path, err := renderObservationRunNote(aggregate, vault)
	if errors.Is(err, errObservationRunNoteUndated) {
		return
	}
	if err == nil {
		err = atomicObservationNote(path, content)
		if err == nil {
			cleanupObservationUndatedRunNotes(vault)
		}
	}
	var now string
	if err == nil {
		now = observationProjectionNow().UTC().Format(time.RFC3339Nano)
		healthPath := filepath.Join(root, "telemetry_health.json")
		data, readErr := os.ReadFile(healthPath)
		health := map[string]any{"schema_version": 1, "status": "OK", "failure_count": 0, "last_error_code": nil}
		if readErr == nil {
			var saved map[string]any
			if json.Unmarshal(data, &saved) == nil && saved != nil {
				health = saved
			}
		}
		health["schema_version"], health["last_projection_write_at"] = 1, now
		encoded, marshalErr := json.Marshal(health)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			err = readErr
		} else if marshalErr != nil {
			err = marshalErr
		} else {
			err = atomicObservationJSON(healthPath, encoded)
		}
	}
	if err == nil {
		var writtenAt *string = &now
		var healthNote string
		var healthPath string
		healthNote, healthPath, err = renderObservationHealthNote(state, vault, writtenAt)
		if err == nil {
			err = atomicObservationNote(healthPath, healthNote)
		}
	}
	if err != nil {
		recordObservationProjectionFailure(state, observationProjectionNow())
		writeObservationHealthFallback(state, vault)
	}
}

func writeObservationHealthFallback(state, vault string) {
	defer func() { _ = recover() }()
	if content, path, err := renderObservationHealthNote(state, vault, nil); err == nil {
		_ = atomicObservationNote(path, content)
	}
}

func updateObservationTask(root, taskID, projectKey string) error {
	indexOffset, consumedSpool, err := readObservationCursor(root, taskID, projectKey)
	if err != nil {
		return err
	}
	indexEvents, nextOffset, err := readObservationTaskIndex(root, taskID, projectKey, indexOffset)
	if err != nil {
		return err
	}
	consumedFromIndex := map[string]bool{}
	events := make([]map[string]any, 0, len(indexEvents))
	for _, event := range indexEvents {
		id := asString(event["event_id"])
		if consumedSpool[id] {
			delete(consumedSpool, id)
			consumedFromIndex[id] = true
		} else {
			events = append(events, event)
		}
	}
	spoolEvents, err := unmergedObservationEvents(root, taskID, projectKey)
	if err != nil {
		return err
	}
	for _, event := range spoolEvents {
		id := asString(event["event_id"])
		if consumedFromIndex[id] || consumedSpool[id] {
			continue
		}
		events = append(events, event)
		consumedSpool[id] = true
	}
	projectRoot := filepath.Join(filepath.Dir(root), "projects", projectKey, "telemetry")
	taskPath := filepath.Join(projectRoot, "tasks", taskID+".json")
	var existing map[string]any
	if data, readErr := os.ReadFile(taskPath); readErr == nil {
		_ = json.Unmarshal(data, &existing)
	}
	aggregate, _ := foldObservationAggregate(existing, taskID, projectKey, events, true)
	if verdict := readObservationVerdict(projectRoot, taskID); verdict != nil {
		aggregate["status"], aggregate["ended_at"] = verdict[0], verdict[1]
	}
	taskJSON, err := canonicalJSON(aggregate)
	if err != nil {
		return err
	}
	if err = atomicObservationJSON(taskPath, taskJSON); err != nil {
		return err
	}
	cursorJSON, err := canonicalJSON(map[string]any{
		"schema_version": 1, "index_offset": nextOffset, "spool_event_ids": sortedObservationIDs(consumedSpool),
	})
	if err != nil {
		return err
	}
	if err = atomicObservationJSON(filepath.Join(projectRoot, "tasks", ".cursors", taskID+".json"), cursorJSON); err != nil {
		return err
	}
	configPath := obsidian.ConfigPath()
	envVault, hasEnv := os.LookupEnv("PURSER_OBSIDIAN_VAULT")
	var envVaultPtr *string
	if hasEnv {
		envVaultPtr = &envVault
	}
	projectObservationTask(root, taskID, projectKey, configPath, envVaultPtr)
	return nil
}

func observationConfigPath() string {
	return obsidian.ConfigPath()
}

func updateObservationTaskBestEffort(root, taskID, projectKey string) {
	if err := updateObservationTask(root, taskID, projectKey); err != nil {
		recordObservationHealth(filepath.Dir(root), "DEGRADED", "TELEMETRY_UPDATE_TASK_FAILED")
	}
}

func readObservationCursor(root, taskID, projectKey string) (int64, map[string]bool, error) {
	path := filepath.Join(filepath.Dir(root), "projects", projectKey, "telemetry", "tasks", ".cursors", taskID+".json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, map[string]bool{}, nil
	}
	if err != nil {
		return 0, nil, err
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil || len(value) != 3 || value["schema_version"] != float64(1) {
		return 0, nil, fmt.Errorf("malformed task telemetry cursor")
	}
	for key := range value {
		if key != "schema_version" && key != "index_offset" && key != "spool_event_ids" {
			return 0, nil, fmt.Errorf("malformed task telemetry cursor")
		}
	}
	offset, ok := value["index_offset"].(float64)
	if !ok || offset < 0 || math.Trunc(offset) != offset || offset >= float64(math.MaxInt64) {
		return 0, nil, fmt.Errorf("malformed task telemetry cursor")
	}
	ids, ok := value["spool_event_ids"].([]any)
	if !ok {
		return 0, nil, fmt.Errorf("malformed task telemetry cursor")
	}
	consumed := make(map[string]bool, len(ids))
	for _, raw := range ids {
		id, ok := raw.(string)
		if !ok {
			return 0, nil, fmt.Errorf("malformed task telemetry cursor")
		}
		consumed[id] = true
	}
	return int64(offset), consumed, nil
}

func readObservationTaskIndex(root, taskID, projectKey string, offset int64) ([]map[string]any, int64, error) {
	path := filepath.Join(root, "events", "by_task", taskID+".jsonl")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if offset != 0 {
			return nil, 0, fmt.Errorf("task telemetry index disappeared")
		}
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if offset > int64(len(data)) {
		return nil, 0, fmt.Errorf("task telemetry index was truncated")
	}
	var events []map[string]any
	consumed := offset
	for remaining := data[offset:]; len(remaining) > 0; {
		newline := bytes.IndexByte(remaining, '\n')
		if newline < 0 {
			break
		}
		line := remaining[:newline]
		consumed += int64(newline + 1)
		var event map[string]any
		if json.Unmarshal(line, &event) == nil && validObservationTaskEvent(event) && event["task_id"] == taskID && event["project_key"] == projectKey {
			events = append(events, event)
		}
		remaining = remaining[newline+1:]
	}
	return events, consumed, nil
}

func validObservationTaskEvent(event map[string]any) bool {
	keys := fields("schema_version event_id ts_utc event_type task_id project_key session_id repo branch agent_id agent_type payload")
	if event == nil || len(event) != len(keys) || event["schema_version"] != float64(1) || !observationUUID.MatchString(asString(event["event_id"])) {
		return false
	}
	for key := range keys {
		if _, ok := event[key]; !ok {
			return false
		}
	}
	ts := asString(event["ts_utc"])
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	_, offset := parsed.Zone()
	_, payloadOK := event["payload"].(map[string]any)
	if len(ts) > 40 || err != nil || offset != 0 || !observationEventTypes[asString(event["event_type"])] || !payloadOK {
		return false
	}
	for _, field := range []string{"task_id", "project_key", "session_id", "repo", "branch", "agent_id", "agent_type"} {
		value := event[field]
		if value == nil {
			continue
		}
		id, ok := value.(string)
		if !ok || !observationID.MatchString(id) {
			return false
		}
		if field == "project_key" && !observationProjectKey.MatchString(id) {
			return false
		}
	}
	return true
}

func unmergedObservationEvents(root, taskID, projectKey string) ([]map[string]any, error) {
	spool := filepath.Join(root, "events", "spool")
	entries, err := os.ReadDir(spool)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var events []map[string]any
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if _, err = os.Stat(filepath.Join(spool, ".merged", id)); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		data, readErr := os.ReadFile(filepath.Join(spool, entry.Name()))
		var event map[string]any
		if readErr == nil && json.Unmarshal(data, &event) == nil && validObservationTaskEvent(event) && event["task_id"] == taskID && event["project_key"] == projectKey {
			events = append(events, event)
		}
	}
	return events, nil
}

func readObservationVerdict(projectRoot, taskID string) []any {
	data, err := os.ReadFile(filepath.Join(projectRoot, "verdicts", taskID+".json"))
	var value map[string]any
	if err != nil || json.Unmarshal(data, &value) != nil || len(value) != 4 || value["schema_version"] != float64(1) || normalizeObservationID(value["task_id"], "unknown") != taskID {
		return nil
	}
	verdict, ok := value["verdict"].(string)
	if !ok || !strings.Contains("|VERIFIED|NOT_VERIFIED|INCONCLUSIVE|NO_CHANGE|", "|"+verdict+"|") {
		return nil
	}
	recorded, ok := value["recorded_at"].(string)
	parsed, parseErr := time.Parse(time.RFC3339Nano, recorded)
	_, zoneOffset := parsed.Zone()
	if !ok || len(recorded) > 40 || parseErr != nil || zoneOffset != 0 {
		return nil
	}
	for key := range value {
		if key != "schema_version" && key != "task_id" && key != "verdict" && key != "recorded_at" {
			return nil
		}
	}
	return []any{verdict, recorded}
}

func sortedObservationIDs(ids map[string]bool) []string {
	values := make([]string, 0, len(ids))
	for id := range ids {
		values = append(values, id)
	}
	sort.Strings(values)
	return values
}

func mergeFields(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, set := range sets {
		for name := range set {
			out[name] = true
		}
	}
	return out
}

var observationRules = map[string]string{}

var reviewerObservationFields = fields("reviewer1_retry_count reviewer1_retry_recovered reviewer1_provider reviewer1_model_requested served_model_attestation duration_ms gemini_usage_known gemini_input_tokens gemini_cached_input_tokens gemini_output_tokens gemini_reasoning_output_tokens gemini_total_tokens gemini_prompt_tokens gemini_completion_tokens gemini_cached_tokens gemini_cache_read_tokens gemini_cache_write_tokens gemini_thinking_tokens")
var reviewerFinishedObservationFields = mergeFields(reviewerObservationFields, fields("reviewer1_result reviewer1_initial_finding_count reviewer1_finding_refs reviewer1_job_id"))
var reviewerFailedObservationFields = mergeFields(reviewerObservationFields, fields("failure_code reviewer1_error_code reviewer1_result reviewer_stdout reviewer_stderr reviewer_exit reviewer_parse_error"))

var observationEventTypes = fields("TASK_ROUTE_SELECTED IMPLEMENTER_STARTED IMPLEMENTER_FINISHED IMPLEMENTER_FAILED REVIEWER1_STARTED REVIEWER1_FINISHED COMPACTION_ADVICE_OUTCOME STOP_BLOCKED SCOPE_CONFLICT_OUTCOME REVIEWER1_SKIPPED REVIEWER1_FAILED ROUTING_PROTOCOL_VIOLATION THREAD_CONTINUITY_VIOLATION UNKNOWN_CONCURRENT_WRITER WORKFLOW_VERDICT_RECORDED TELEMETRY_WARNING CLAUDE_TASK_USAGE_RECORDED CONTEXT_IO_TURN_RECORDED CODEX_RESULT_UNFETCHED CLAUDE_DECISION_RECORDED WORKER_REPLAY_CONFLICT MECHANICAL_EDIT_OBSERVED MAINTENANCE_REPORT_FINISHED MAINTENANCE_REPORT_ACTED READINESS_CHECK_OUTCOME PLAN_REDIRECT")

const claudeUsageSnapshotLimit = 16 << 10

var claudeUsagePayloadFields = fields("claude_usage_session_id claude_usage_captured_at claude_5h_start_pct claude_5h_end_pct claude_5h_resets_at claude_7d_start_pct claude_7d_end_pct claude_7d_resets_at claude_context_used_percentage claude_context_window_size claude_context_total_tokens claude_context_input_tokens claude_context_output_tokens claude_cache_creation_input_tokens claude_cache_read_input_tokens claude_session_estimated_cost_usd claude_session_total_duration_ms claude_usage_known claude_rate_limit_delta_scope")

func readClaudeUsagePayload(stateRoot string, now time.Time, eventType string) map[string]any {
	unknown := map[string]any{"claude_usage_known": false, "claude_rate_limit_delta_scope": "account_window_observation"}
	file, err := os.Open(filepath.Join(stateRoot, "telemetry", "latest", "claude_usage.json"))
	if err != nil {
		return unknown
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, claudeUsageSnapshotLimit+1))
	if err != nil || len(data) > claudeUsageSnapshotLimit {
		return unknown
	}
	var snapshot map[string]any
	if json.Unmarshal(data, &snapshot) != nil || snapshot == nil || snapshot["schema_version"] != float64(1) {
		return unknown
	}
	capturedText, ok := snapshot["captured_at"].(string)
	capturedAt, valid := parseClaudeUsageTimestamp(capturedText)
	if !ok || !valid || capturedAt.After(now) || now.Sub(capturedAt) > 5*time.Minute {
		return unknown
	}
	payload := map[string]any{"claude_usage_captured_at": capturedText, "claude_usage_known": false, "claude_rate_limit_delta_scope": "account_window_observation"}
	if session, ok := snapshot["session_id"].(string); ok && observationID.MatchString(session) {
		payload["claude_usage_session_id"] = session
	}
	for _, field := range []struct{ source, target string }{
		{"context_used_percentage", "claude_context_used_percentage"},
		{"context_window_size", "claude_context_window_size"},
		{"context_total_tokens", "claude_context_total_tokens"},
		{"context_input_tokens", "claude_context_input_tokens"},
		{"context_output_tokens", "claude_context_output_tokens"},
		{"cache_creation_input_tokens", "claude_cache_creation_input_tokens"},
		{"cache_read_input_tokens", "claude_cache_read_input_tokens"},
		{"session_estimated_cost_usd", "claude_session_estimated_cost_usd"},
		{"session_total_duration_ms", "claude_session_total_duration_ms"},
	} {
		if number := validClaudeNumber(snapshot[field.source]); number != nil {
			payload[field.target], payload["claude_usage_known"] = number, true
		}
	}
	for _, window := range []struct{ name, prefix string }{{"five_hour", "claude_5h"}, {"seven_day", "claude_7d"}} {
		values, _ := snapshot[window.name].(map[string]any)
		if values == nil {
			continue
		}
		if number := validClaudeNumber(values["used_percentage"]); number != nil {
			endpoint := "start"
			if eventType == "IMPLEMENTER_FINISHED" || eventType == "IMPLEMENTER_FAILED" {
				endpoint = "end"
			}
			payload[window.prefix+"_"+endpoint+"_pct"], payload["claude_usage_known"] = number, true
		}
		if reset, ok := values["resets_at"].(string); ok {
			if _, valid := parseClaudeUsageTimestamp(reset); valid {
				payload[window.prefix+"_resets_at"] = reset
			}
		}
	}
	return payload
}

func parseClaudeUsageTimestamp(value string) (time.Time, bool) {
	if len(value) > 40 {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	_, offset := parsed.Zone()
	return parsed, offset == 0
}

func validClaudeNumber(value any) any {
	number, ok := value.(float64)
	if !ok || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return number
}

func init() {
	setObservationRule("bool", "routing_enforced fresh_thread reply_call claude_usage_known")
	setObservationRule("number", "duration_ms longest_silent_gap_seconds")
	setObservationRule("int", "worker_prompt_chars scope_path_count non_test_scope_count")
	setObservationRule("number", "queue_wait_ms claude_5h_start_pct claude_5h_end_pct claude_7d_start_pct claude_7d_end_pct claude_context_used_percentage claude_context_window_size claude_context_total_tokens claude_context_input_tokens claude_context_output_tokens claude_cache_creation_input_tokens claude_cache_read_input_tokens claude_session_estimated_cost_usd claude_session_total_duration_ms")
	setObservationRule("enum:implementation operation plan", "phase")
	setObservationRule("enum:mechanical subtle normal consequential escalated operation plan_standard", "route previous_route new_route")
	setObservationRule("enum:no_open_design_decision_or_user_request", "route_adjustment_reason")
	setObservationRule("enum:override route", "model_source")
	setObservationRule("enum:low medium high xhigh none", "effort")
	setObservationRule("enum:new_task continue fix route_change reanchor repeated_failure user_override", "reason")
	setObservationRule("enum:CODEX_CALL_FAILED CODEX_CALL_INTERRUPTED REVIEWER1_UNAVAILABLE REVIEWER1_INCOMPLETE", "failure_code")
	setObservationRule("enum:NO_MATERIAL_FINDING_IN_REVIEWED_SCOPE CHANGES_REQUIRED UNCERTAIN REVIEWER1_UNAVAILABLE", "reviewer1_result")
	setObservationRule("enum:PHASE_OPERATION NO_DELTA BELOW_THRESHOLD REVIEWER_UNAVAILABLE", "reviewer1_skipped_reason")
	setObservationRule("enum:UNAVAILABLE", "served_model_attestation")
	setObservationRule("bool", "gemini_usage_known reviewer1_retry_recovered")
	setObservationRule("int", "reviewer1_retry_count reviewer1_initial_finding_count reviewer1_valid_count reviewer1_false_count")
	setObservationRule("number", "gemini_input_tokens gemini_cached_input_tokens gemini_output_tokens gemini_reasoning_output_tokens gemini_total_tokens gemini_prompt_tokens gemini_completion_tokens gemini_cached_tokens gemini_cache_read_tokens gemini_cache_write_tokens gemini_thinking_tokens")
	setObservationRule("id", "reviewer1_provider reviewer1_model_requested reviewer1_error_code")
	setObservationRule("enum:explicit inherited", "reason_source")
	setObservationRule("enum:implementation operation plan unknown counting_status", "task_type")
	setObservationRule("enum:accepted send_back rejected unknown", "decision")
	setObservationRule("enum:REVIEWER1_REFERENCED NON_REVIEWER1_OR_UNATTRIBUTED STALE_OR_CROSS_JOB", "review_attribution")
	setObservationRule("finding_refs", "review_finding_refs")
	setObservationRule("finding_refs", "reviewer1_finding_refs")
	setObservationRule("finding_refs", "reviewer1_answered_finding_refs")
	setObservationRule("int", "reviewer1_answered_finding_count")
	setObservationRule("enum:UNMET NO_UNMET_FOUND UNKNOWN UNAVAILABLE", "verdict")
	setObservationRule("id", "code")
	setObservationRule("id", "implementer agent provider claude_usage_session_id job_id reviewer1_job_id")
	setObservationRule("enum:denied queued promoted applied resolved_by_hand discarded expired", "action")
	setObservationRule("time", "claude_usage_captured_at claude_5h_resets_at claude_7d_resets_at")
	setObservationRule("scope", "claude_rate_limit_delta_scope")
	setObservationRule("evidence", "reviewer_stdout reviewer_stderr reviewer_exit reviewer_parse_error")
	setObservationRule("id", "report_id reason_code")
	setObservationRule("enum:cleanup test-gaps", "report_type")
	setObservationRule("enum:completed failed yielded", "outcome")
	setObservationRule("int", "findings_high_count findings_medium_count findings_low_count")
	setObservationRule("number", "elapsed_seconds quota_change_active_window_pct line_coverage_pct")
}

func setObservationRule(rule, names string) {
	for _, name := range strings.Fields(names) {
		observationRules[name] = rule
	}
}

func normalizeObservationID(value any, fallback string) string {
	original, _ := value.(string)
	clean := strings.Trim(unsafeObservationID.ReplaceAllString(original, "_"), ". ")
	if len(clean) > 96 {
		clean = clean[:96]
	}
	changed := clean != original || clean == ""
	base := strings.ToUpper(strings.SplitN(clean, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		changed, clean = true, "_"+clean
	}
	if clean == "" {
		clean = fallback
	}
	if changed {
		sum := sha256.Sum256([]byte(original))
		if len(clean) > 87 {
			clean = clean[:87]
		}
		clean += "-" + hex.EncodeToString(sum[:4])
	}
	return clean
}

func buildObservation(eventType string, envelope, payload map[string]any, taskID any) (map[string]any, error) {
	return buildObservationAtStateRoot(eventType, envelope, payload, taskID, "")
}

// EmitMaintenanceReportObservation routes maintenance telemetry through the
// same observation schema, locking, spool, and append writer as hook events.
func EmitMaintenanceReportObservation(stateRoot, repository, eventType string, payload map[string]any) error {
	project := contextio.ProjectKey(repository)
	event, err := buildObservationAtStateRoot(eventType, map[string]any{
		"project_key": project,
	}, payload, nil, stateRoot)
	if err != nil {
		return err
	}
	_, err = persistObservation(filepath.Join(stateRoot, "telemetry"), event)
	return err
}

func buildObservationAtStateRoot(eventType string, envelope, payload map[string]any, taskID any, stateRoot string) (map[string]any, error) {
	allowed, ok := observationFields[eventType]
	if !ok {
		return nil, fmt.Errorf("unknown telemetry event type")
	}
	if (eventType == "IMPLEMENTER_STARTED" || eventType == "IMPLEMENTER_FINISHED" || eventType == "IMPLEMENTER_FAILED") && stateRoot != "" {
		callerPayload := payload
		payload = make(map[string]any, len(callerPayload)+2)
		for name, value := range callerPayload {
			payload[name] = value
		}
		for name := range claudeUsagePayloadFields {
			delete(payload, name)
		}
		for name, value := range readClaudeUsagePayload(stateRoot, observationNow().UTC(), eventType) {
			payload[name] = value
		}
	}
	clean := map[string]any{}
	for name, value := range payload {
		if !allowed[name] {
			return nil, fmt.Errorf("telemetry payload contains an unregistered field")
		}
		if value == nil && name != "eta_minutes" && !(eventType == "MAINTENANCE_REPORT_FINISHED" && (name == "quota_change_active_window_pct" || name == "findings_high_count" || name == "findings_medium_count" || name == "findings_low_count" || name == "line_coverage_pct")) {
			continue
		}
		if err := validateObservationValue(name, value); err != nil {
			return nil, err
		}
		clean[name] = value
	}
	if attribution, ok := clean["review_attribution"].(string); ok {
		refs, _ := clean["review_finding_refs"].([]any)
		if attribution == "NON_REVIEWER1_OR_UNATTRIBUTED" && len(refs) != 0 || attribution != "NON_REVIEWER1_OR_UNATTRIBUTED" && len(refs) == 0 {
			return nil, fmt.Errorf("review attribution does not match finding references")
		}
		if refs == nil {
			clean["review_finding_refs"] = []any{}
		}
	}
	project := envelope["project_key"]
	if project == nil && taskID != nil {
		project = "unresolved"
	}
	if project != nil {
		p, ok := project.(string)
		if !ok || !observationProjectKey.MatchString(p) {
			return nil, fmt.Errorf("telemetry project key is invalid")
		}
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	return map[string]any{"schema_version": observationSchemaVersion, "event_id": fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), "ts_utc": time.Now().UTC().Format(time.RFC3339Nano), "event_type": eventType,
		"task_id": optionalObservationID(taskID, "unknown"), "project_key": project, "session_id": optionalObservationID(envelope["session_id"], "session"), "repo": optionalObservationID(envelope["repo"], "repo"), "branch": optionalObservationID(envelope["branch"], "branch"), "agent_id": optionalObservationID(envelope["agent_id"], "agent"), "agent_type": optionalObservationID(envelope["agent_type"], "agent"), "payload": clean}, nil
}

func optionalObservationID(value any, fallback string) any {
	if value == nil {
		return nil
	}
	return normalizeObservationID(value, fallback)
}
func validNumber(value any, integer bool) bool {
	var n float64
	switch x := value.(type) {
	case json.Number:
		parsed, err := x.Float64()
		if err != nil {
			return false
		}
		n = parsed
	case float64:
		n = x
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	default:
		return false
	}
	return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) && (!integer || math.Trunc(n) == n)
}
func oneOf(value any, values string) bool {
	for _, item := range strings.Fields(values) {
		if value == item {
			return true
		}
	}
	return false
}

func validateObservationValue(name string, value any) error {
	rule := observationRules[name]
	valid := false
	switch {
	case name == "quota_change_active_window_pct" || name == "findings_high_count" || name == "findings_medium_count" || name == "findings_low_count" || name == "line_coverage_pct":
		valid = value == nil || (name == "findings_high_count" || name == "findings_medium_count" || name == "findings_low_count") && validNumber(value, true) || (name == "quota_change_active_window_pct" || name == "line_coverage_pct") && validNumber(value, false)
	case name == "reviewer1_retry_count":
		valid = validNumber(value, true) && oneOfNumeric(value, 0, 1)
	case rule == "bool":
		_, valid = value.(bool)
	case rule == "int":
		valid = validNumber(value, true)
	case rule == "number":
		valid = validNumber(value, false)
	case rule == "id":
		s, ok := value.(string)
		valid = ok && observationID.MatchString(s)
	case rule == "finding_refs":
		items, ok := value.([]any)
		valid = ok && (name == "reviewer1_answered_finding_refs" || len(items) <= 20)
		seen := map[string]bool{}
		for _, item := range items {
			s, ok := item.(string)
			if !ok || !observationFindingID.MatchString(s) || seen[s] {
				valid = false
				break
			}
			seen[s] = true
		}
	case rule == "model":
		s, ok := value.(string)
		valid = ok && modelValue.MatchString(s)
	case rule == "string_map_list":
		items, ok := value.([]any)
		valid = ok
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok || len(m) != 2 || m["path"] == "" || m["task_id"] == "" {
				valid = false
				break
			}
			if _, ok = m["path"].(string); !ok {
				valid = false
				break
			}
			if _, ok = m["task_id"].(string); !ok {
				valid = false
				break
			}
		}
	case rule == "time":
		s, ok := value.(string)
		parsed, err := time.Parse(time.RFC3339Nano, s)
		_, offset := parsed.Zone()
		valid = ok && len(s) <= 40 && err == nil && offset == 0
	case rule == "evidence":
		s, ok := value.(string)
		valid = ok && len(s) <= 4200
	case strings.HasPrefix(rule, "enum:"):
		valid = oneOf(value, strings.TrimPrefix(rule, "enum:"))
	case name == "model":
		s, ok := value.(string)
		valid = ok && modelValue.MatchString(s)
	case name == "violation_code":
		valid = oneOf(value, "MALFORMED_TOOL_INPUT MISSING_ORCH_LINE MALFORMED_ORCH_LINE UNKNOWN_KEY MISSING_REQUIRED_KEY INVALID_ENUM_VALUE INVALID_SCOPE VAGUE_SOL_BASIS UNBOUNDED_SCOPE CODEX_REPLY_INPUT_INVALID THREAD_ID_MISMATCH TASK_ID_MISMATCH ROUTE_MISMATCH MODEL_MISMATCH EFFORT_MISMATCH THREAD_TASK_MISMATCH THREAD_PHASE_MISMATCH THREAD_ROUTE_MISMATCH USE_CODEX_REPLY NO_LIVE_THREAD INVALID_LIVE_THREAD INVALID_REQUEST SCOPE_LEASE_CONFLICT LEASE_ADMISSION_UNCERTAIN LEASE_OWNER_CONFLICT LEASE_INVALID CODEX_INPUT_REWRITE_UNDELIVERED")
	case name == "eta_minutes":
		valid = value == nil || validNumber(value, true) && oneOfNumeric(value, 1, 600)
	case name == "suspected_route_mismatch":
		m, ok := value.(map[string]any)
		valid = ok && len(m) == 2 && oneOf(m["declared_route"], "mechanical subtle normal consequential escalated operation plan_standard") && oneOf(m["derived_type"], "counting_status operation")
	case name == "claude_rate_limit_delta_scope":
		valid = value == "account_window_observation"
	case name == "decision":
		valid = oneOf(value, "accepted send_back rejected unknown")
	case name == "source":
		m, ok := value.(map[string]any)
		kind, kindOK := m["kind"].(string)
		jobID, jobOK := m["job_id"].(string)
		valid = ok && len(m) == 2 && kindOK && oneOf(kind, "apply reply discard result redispatch") && jobOK && observationID.MatchString(jobID)
	}
	if !valid {
		return fmt.Errorf("invalid telemetry field %q", name)
	}
	return nil
}

func oneOfNumeric(value any, min, max float64) bool {
	switch n := value.(type) {
	case int:
		return float64(n) >= min && float64(n) <= max
	case float64:
		return n >= min && n <= max
	}
	return false
}

var healthErrorCodes = fields("TELEMETRY_ENTRYPOINT_FAILURE TELEMETRY_EMIT_FAILED TELEMETRY_UPDATE_TASK_FAILED TELEMETRY_REBUILD_FAILED TELEMETRY_HEALTH_FAILED TELEMETRY_SPOOL_MERGE_FAILED TELEMETRY_PROJECTION_FAILED MALFORMED_SPOOL_EVENT CLAUDE_TRANSCRIPT_PARSER_UNAVAILABLE CLAUDE_TRANSCRIPT_PARSER_FAILED CLAUDE_TRANSCRIPT_FINALIZATION_FAILED CONTEXT_IO_EMIT_FAILED")

// recordObservationHealth is best effort: health IO cannot affect enforcement.
func recordObservationHealth(root, status, errorCode string) {
	defer func() { _ = recover() }()
	path := filepath.Join(root, "telemetry", "telemetry_health.json")
	data, err := os.ReadFile(path)
	health := map[string]any{"schema_version": 1, "status": "OK", "failure_count": 0, "last_error_code": nil}
	if err == nil {
		var saved map[string]any
		if json.Unmarshal(data, &saved) == nil && saved != nil {
			health = saved
		}
	}
	if status == "DEGRADED" {
		if !healthErrorCodes[errorCode] {
			errorCode = "TELEMETRY_HEALTH_FAILED"
		}
		count, ok := health["failure_count"].(float64)
		if !ok {
			if n, yes := health["failure_count"].(int); yes {
				count, ok = float64(n), true
			} else if value, yes := health["failure_count"].(bool); yes {
				if value {
					count = 1
				}
				ok = true
			}
		}
		if !ok || count < 0 || math.Trunc(count) != count {
			count = 0
		}
		health["schema_version"], health["status"], health["failure_count"], health["last_error_code"] = 1, status, count+1, errorCode
	} else if status == "OK" {
		health["schema_version"], health["status"], health["last_error_code"] = 1, status, nil
	} else {
		return
	}
	health["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err = json.Marshal(health)
	if err != nil {
		return
	}
	_ = atomicWrite(path, data)
}
