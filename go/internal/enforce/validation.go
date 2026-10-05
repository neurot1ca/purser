package enforce

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"
)

const implementerReportLimit = 1200

var allowedValidationKeys = map[string]bool{"source": true, "status": true, "summary": true, "commands": true, "exit_codes": true, "artifacts": true}

func canonicalUnknownValidation(summary *string) map[string]any {
	target := "No verifiable validation evidence was provided."
	if summary != nil && strings.TrimSpace(*summary) != "" {
		target = strings.TrimSpace(*summary)
	}
	record := unknownValidationRecord(target)
	if pythonJSONSize(record) <= implementerReportLimit {
		return record
	}
	runes := []rune(target)
	low, high, best := 0, len(runes), ""
	for low <= high {
		mid := (low + high) / 2
		record["summary"] = string(runes[:mid])
		if pythonJSONSize(record) <= implementerReportLimit {
			best = string(runes[:mid])
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	record["summary"] = best
	return record
}
func unknownValidationRecord(summary string) map[string]any {
	return map[string]any{"source": "WORKER_REPORTED", "status": "UNKNOWN", "summary": summary, "commands": []string{}, "exit_codes": []any{}, "artifacts": []string{}}
}
func pythonJSONSize(record map[string]any) int {
	b, _ := json.Marshal(record)
	b = bytes.ReplaceAll(b, []byte(`\u003c`), []byte("<"))
	b = bytes.ReplaceAll(b, []byte(`\u003e`), []byte(">"))
	b = bytes.ReplaceAll(b, []byte(`\u0026`), []byte("&"))
	b = bytes.ReplaceAll(b, []byte(`\u2028`), []byte(" "))
	b = bytes.ReplaceAll(b, []byte(`\u2029`), []byte(" "))
	quoted, escaped, separators := false, false, 0
	for i := 0; i < len(b); i++ {
		if quoted && escaped {
			escaped = false
			continue
		}
		if quoted && b[i] == '\\' {
			escaped = true
			continue
		}
		if b[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted && (b[i] == ':' || b[i] == ',') {
			separators++
		}
	}
	return len(b) + separators
}
func splitPythonLines(report string) []string {
	lines := make([]string, 0, 1)
	start := 0
	for i := 0; i < len(report); {
		r, size := utf8.DecodeRuneInString(report[i:])
		if strings.ContainsRune("\n\v\f\x1c\x1d\x1e\u0085\u2028\u2029", r) || r == '\r' {
			lines = append(lines, report[start:i])
			i += size
			if r == '\r' && i < len(report) && report[i] == '\n' {
				i++
			}
			start = i
			continue
		}
		i += size
	}
	if start < len(report) {
		lines = append(lines, report[start:])
	}
	return lines
}
func parseWorkerValidation(report string) map[string]any {
	if strings.TrimSpace(report) == "" {
		s := "No implementer report was provided."
		return canonicalUnknownValidation(&s)
	}
	lines := make([]string, 0, 1)
	for _, line := range splitPythonLines(report) {
		line = strings.TrimSpace(line)
		if len(line) >= 2 && strings.HasPrefix(line, "`") && strings.HasSuffix(line, "`") {
			line = strings.TrimSpace(line[1 : len(line)-1])
		}
		payload := strings.TrimPrefix(line, "VALIDATION:")
		if strings.HasPrefix(line, "VALIDATION:") && strings.TrimSpace(payload) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		lower := strings.ToLower(report)
		idx := strings.Index(lower, "validation:")
		summary := strings.TrimSpace(report)
		if idx >= 0 {
			summary = strings.TrimSpace(report[idx:])
		}
		return canonicalUnknownValidation(&summary)
	}
	if len(lines) > 1 {
		s := "Multiple conflicting VALIDATION records were reported."
		return canonicalUnknownValidation(&s)
	}
	line := lines[0]
	reject := func(reason string) map[string]any {
		s := reason + ": " + line
		return canonicalUnknownValidation(&s)
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "VALIDATION:"))
	var data any
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return reject("invalid_json")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return reject("invalid_json")
	}
	object, ok := data.(map[string]any)
	if !ok {
		return reject("not_object")
	}
	unknown := make([]string, 0)
	for key := range object {
		if !allowedValidationKeys[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return reject("unknown_keys=" + strings.Join(unknown, ","))
	}
	if object["source"] != "WORKER_REPORTED" {
		return reject("bad_source")
	}
	status, ok := object["status"].(string)
	if !ok || (status != "PASS" && status != "FAIL" && status != "UNKNOWN") {
		return reject("bad_status")
	}
	cleanSummary := "Structured worker validation reported " + status + "."
	if raw, exists := object["summary"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return reject("bad_summary")
		}
		cleanSummary = strings.TrimSpace(value)
	}
	cleanCommands, ok := cleanStringList(object, "commands")
	if !ok {
		return reject("bad_commands")
	}
	cleanArtifacts, ok := cleanStringList(object, "artifacts")
	if !ok {
		return reject("bad_artifacts")
	}
	cleanCodes := make([]any, 0)
	if raw, exists := object["exit_codes"]; exists {
		values, ok := raw.([]any)
		if !ok {
			return reject("bad_exit_codes")
		}
		for _, value := range values {
			number, ok := value.(json.Number)
			if !ok {
				return reject("bad_exit_codes")
			}
			integer := new(big.Int)
			if _, ok := integer.SetString(number.String(), 10); !ok {
				return reject("bad_exit_codes")
			}
			cleanCodes = append(cleanCodes, json.Number(integer.String()))
		}
	}
	if len(cleanCommands) != len(cleanCodes) {
		return reject("commands_exit_codes_mismatch")
	}
	if status == "PASS" {
		if len(cleanCommands) == 0 {
			return reject("pass_without_commands")
		}
		for _, code := range cleanCodes {
			if code.(json.Number).String() != "0" {
				return reject("pass_with_nonzero_exit")
			}
		}
	} else if status == "FAIL" {
		if len(cleanCommands) == 0 {
			return reject("fail_without_commands")
		}
		nonzero := false
		for _, code := range cleanCodes {
			if code.(json.Number).String() != "0" {
				nonzero = true
			}
		}
		if !nonzero {
			return reject("fail_without_nonzero_exit")
		}
	}
	candidate := map[string]any{"source": "WORKER_REPORTED", "status": status, "summary": cleanSummary, "commands": cleanCommands, "exit_codes": cleanCodes, "artifacts": cleanArtifacts}
	if pythonJSONSize(candidate) > implementerReportLimit {
		return reject("record_too_large")
	}
	return candidate
}
func cleanStringList(object map[string]any, key string) ([]string, bool) {
	clean := make([]string, 0)
	raw, exists := object[key]
	if !exists {
		return clean, true
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	for _, value := range values {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, false
		}
		clean = append(clean, strings.TrimSpace(text))
	}
	return clean, true
}
