package review

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var packetFields = []string{"objective", "acceptance_criteria", "authority_contract", "delegated_diff", "validation_summary"}

const diffRedactionMarker = "[[REDACTED: PRE-EXISTING OR REMOVED SENSITIVE SPAN]]"

var forbiddenText = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^\s*(implementer narrative|hidden reasoning|claude conversation|full success logs?|whole[- ]repository state|raw agent output)\s*:`),
	regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----`),
	regexp.MustCompile(`\b(sk[-_]|ghp_|github_pat_)[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{12,}`),
	regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|client[_-]?secret|private[_-]?key|password)\s*[:=]\s*['"]?[A-Za-z0-9_./+\-=]{12,}`),
}

type PacketReady struct {
	Packet              map[string]string
	Encoded             []byte
	AddedRedactionCount int
	AddedAffectedFiles  []string
	PartialCoverage     bool
}

type PacketOmitted struct {
	Code    string
	Message string
	Omitted []string
}

type PacketBuild interface{ isPacketBuild() }

func (PacketReady) isPacketBuild()   {}
func (PacketOmitted) isPacketBuild() {}

// BuildReviewPacket enforces the Python Reviewer-1 packet allowlist and byte budget.
func BuildReviewPacket(materials map[string]any, maxBytes int) PacketBuild {
	if materials == nil {
		return PacketOmitted{"REVIEW_PACKET_INVALID", "The review packet source was not a mapping; no context was sent.", append([]string(nil), packetFields...)}
	}
	keys := make([]string, 0, len(materials))
	for key := range materials {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	extras := make([]string, 0)
	for _, key := range keys {
		if !containsString(packetFields, key) {
			extras = append(extras, key)
		}
	}
	missing := make([]string, 0)
	for _, field := range packetFields {
		if _, ok := materials[field]; !ok {
			missing = append(missing, field)
		}
	}
	if len(extras) != 0 || len(missing) != 0 {
		details := make([]string, 0, 2)
		if len(extras) != 0 {
			details = append(details, "forbidden or unrelated fields: "+strings.Join(extras, ", "))
		}
		if len(missing) != 0 {
			details = append(details, "missing required fields: "+strings.Join(missing, ", "))
		}
		omitted := missing
		if len(omitted) == 0 {
			omitted = extras
		}
		return PacketOmitted{"REVIEW_PACKET_FIELDS_REJECTED", "The packet allowlist rejected " + strings.Join(details, "; ") + "; no context was sent.", omitted}
	}
	if maxBytes <= 0 {
		return PacketOmitted{"REVIEW_PACKET_BUDGET_INVALID", "The packet byte budget was invalid; no context was sent.", append([]string(nil), packetFields...)}
	}

	packet := make(map[string]string, len(packetFields))
	redactionCount, addedRedactionCount := 0, 0
	var affectedFiles, addedAffectedFiles []string
	for _, field := range packetFields {
		value, ok := materials[field].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return PacketOmitted{"REVIEW_PACKET_VALUE_INVALID", fmt.Sprintf("The required packet field %q was empty or non-text; no context was sent.", field), []string{field}}
		}
		if field == "delegated_diff" {
			redacted, ok := redactDelegatedDiff(value)
			if !ok {
				return PacketOmitted{"REVIEW_PACKET_SENSITIVE_CONTENT", "Credential-like or excluded narrative material was detected in 'delegated_diff'; it was not sent.", []string{field}}
			}
			value, redactionCount, affectedFiles, addedRedactionCount, addedAffectedFiles = redacted.text, redacted.count, redacted.files, redacted.addedCount, redacted.addedFiles
		}
		if containsForbiddenText(value) {
			return PacketOmitted{"REVIEW_PACKET_SENSITIVE_CONTENT", fmt.Sprintf("Credential-like or excluded narrative material was detected in %q; it was not sent.", field), []string{field}}
		}
		packet[field] = value
	}
	if redactionCount != 0 {
		materialDescription := "pre-existing context or removed credential-shaped/excluded material"
		if addedRedactionCount != 0 {
			materialDescription = "added credential-shaped material"
		}
		packet["validation_summary"] += fmt.Sprintf("\n\nREDACTION NOTICE: %d forbidden-text span(s) were redacted from %s in: %s. Each unmistakable %s marker replaces the complete matched span. A redaction is not permission to ignore a possible defect; question any unexplained redaction.", redactionCount, materialDescription, strings.Join(affectedFiles, ", "), diffRedactionMarker)
	}
	encoded, err := packetCanonicalJSON(packet)
	if err != nil {
		return PacketOmitted{"REVIEW_PACKET_INVALID", "The review packet could not be encoded; no context was sent.", append([]string(nil), packetFields...)}
	}
	if len(encoded) > maxBytes {
		diffBytes := len([]byte(materials["delegated_diff"].(string)))
		if partial, ok := boundedPartialPacket(packet, maxBytes); ok {
			return partial
		}
		omitted := append([]string(nil), packetFields...)
		if diffBytes > maxBytes {
			omitted = []string{"delegated_diff (complete diff exceeds packet budget; not truncated)"}
		}
		return PacketOmitted{"REVIEW_PACKET_TOO_LARGE", fmt.Sprintf("The complete %d-byte packet exceeds the %d-byte budget. Omitted: %s. No partial diff was sent and no model call was made.", len(encoded), maxBytes, strings.Join(omitted, ", ")), omitted}
	}
	return PacketReady{Packet: packet, Encoded: encoded, AddedRedactionCount: addedRedactionCount, AddedAffectedFiles: addedAffectedFiles}
}

func boundedPartialPacket(packet map[string]string, maxBytes int) (PacketReady, bool) {
	const marker = "\n\n[[PARTIAL COVERAGE: bounded diff excerpt; remaining delegated changes omitted. FULL review required.]]"
	sections := diffFileSections(packet["delegated_diff"])
	var best PacketReady
	var selected strings.Builder
	for _, section := range sections {
		candidate := make(map[string]string, len(packet))
		for key, value := range packet {
			candidate[key] = value
		}
		candidate["delegated_diff"] = selected.String() + section + marker
		encoded, err := packetCanonicalJSON(candidate)
		if err == nil && len(encoded) <= maxBytes {
			best = PacketReady{Packet: candidate, Encoded: encoded, PartialCoverage: true}
			selected.WriteString(section)
		}
	}
	return best, best.Packet != nil
}

func diffFileSections(diff string) []string {
	var sections []string
	var section strings.Builder
	inSection := false
	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if inSection {
				sections = append(sections, section.String())
				section.Reset()
			}
			inSection = true
		}
		if inSection {
			section.WriteString(line)
		}
	}
	if inSection {
		sections = append(sections, section.String())
	}
	return sections
}

type diffRedaction struct {
	text       string
	count      int
	files      []string
	addedCount int
	addedFiles []string
}

type textSpan struct{ start, end int }

func containsForbiddenText(value string) bool {
	for _, pattern := range forbiddenText {
		if pattern.FindStringIndex(value) != nil {
			return true
		}
	}
	return false
}

func containsExcludedNarrative(value string) bool {
	return forbiddenText[0].FindStringIndex(value) != nil
}

func forbiddenSpans(value string) []textSpan {
	spans := make([]textSpan, 0)
	for _, pattern := range forbiddenText {
		for _, indices := range pattern.FindAllStringIndex(value, -1) {
			spans = append(spans, textSpan{indices[0], indices[1]})
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start == spans[j].start {
			return spans[i].end < spans[j].end
		}
		return spans[i].start < spans[j].start
	})
	merged := spans[:0]
	for _, span := range spans {
		if len(merged) != 0 && span.start < merged[len(merged)-1].end {
			if span.end > merged[len(merged)-1].end {
				merged[len(merged)-1].end = span.end
			}
		} else {
			merged = append(merged, span)
		}
	}
	return merged
}

func diffHeaderPath(value string) string {
	if value == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/") {
		return value[2:]
	}
	return value
}

func redactDelegatedDiff(diff string) (diffRedaction, bool) {
	var output strings.Builder
	affected, addedAffected := map[string]bool{}, map[string]bool{}
	count, addedCount := 0, 0
	oldPath, currentPath := "", ""
	inHunk := false
	for _, rawLine := range splitLinesKeepEnds(diff) {
		line, ending := splitLineEnding(rawLine)
		kind, content := byte(0), line
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldPath, currentPath, inHunk = "", "", false
		case !inHunk && strings.HasPrefix(line, "--- "):
			oldPath = diffHeaderPath(line[4:])
		case !inHunk && strings.HasPrefix(line, "+++ "):
			currentPath = diffHeaderPath(line[4:])
			if currentPath == "" {
				currentPath = oldPath
			}
		case strings.HasPrefix(line, "@@ "):
			inHunk = true
		case inHunk && len(line) != 0 && (line[0] == '+' || line[0] == '-' || line[0] == ' '):
			kind, content = line[0], line[1:]
		}
		spans := forbiddenSpans(content)
		if len(spans) == 0 {
			output.WriteString(rawLine)
			continue
		}
		if containsExcludedNarrative(content) || kind == 0 || currentPath == "" {
			return diffRedaction{}, false
		}
		output.WriteByte(kind)
		cursor := 0
		for _, span := range spans {
			output.WriteString(content[cursor:span.start])
			output.WriteString(diffRedactionMarker)
			cursor = span.end
		}
		output.WriteString(content[cursor:])
		output.WriteString(ending)
		count += len(spans)
		affected[currentPath] = true
		if kind == '+' {
			addedCount += len(spans)
			addedAffected[currentPath] = true
		}
	}
	return diffRedaction{output.String(), count, sortedKeys(affected), addedCount, sortedKeys(addedAffected)}, true
}

func splitLinesKeepEnds(value string) []string {
	lines := make([]string, 0)
	start := 0
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		lineEnd := r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x1c || r == 0x1d || r == 0x1e || r == 0x85 || r == 0x2028 || r == 0x2029
		if lineEnd {
			i += size
			if r == '\r' && i < len(value) && value[i] == '\n' {
				i++
			}
			lines = append(lines, value[start:i])
			start = i
		} else {
			i += size
		}
	}
	if start < len(value) {
		lines = append(lines, value[start:])
	}
	return lines
}

func splitLineEnding(value string) (string, string) {
	if strings.HasSuffix(value, "\r\n") {
		return value[:len(value)-2], "\r\n"
	}
	if len(value) == 0 {
		return value, ""
	}
	for _, ending := range []string{"\n", "\r", "\v", "\f", "\x1c", "\x1d", "\x1e", "\u0085", "\u2028", "\u2029"} {
		if strings.HasSuffix(value, ending) {
			return value[:len(value)-len(ending)], ending
		}
	}
	return value, ""
}

func packetCanonicalJSON(packet map[string]string) ([]byte, error) {
	// review.go's canonicalJSON escapes U+2028/U+2029; Python ensure_ascii=False
	// emits them literally, so packet strings use this small Python-compatible encoder.
	keys := make([]string, 0, len(packet))
	for key := range packet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var output strings.Builder
	output.WriteByte('{')
	for i, key := range keys {
		if i != 0 {
			output.WriteByte(',')
		}
		writePythonJSONString(&output, key)
		output.WriteByte(':')
		writePythonJSONString(&output, packet[key])
	}
	output.WriteByte('}')
	return []byte(output.String()), nil
}

func writePythonJSONString(output *strings.Builder, value string) {
	output.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			output.WriteString(`\"`)
		case '\\':
			output.WriteString(`\\`)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(output, `\u%04x`, r)
			} else {
				output.WriteRune(r)
			}
		}
	}
	output.WriteByte('"')
}

// WritePacket creates a private, exclusive packet file in directory. A non-empty
// name selects the fixed OpenCode path; an empty name uses Python's temp-file prefix.
func WritePacket(directory, name string, encoded []byte) (string, error) {
	var file *os.File
	var err error
	temporary := name == ""
	if temporary {
		file, err = os.CreateTemp(directory, ".reviewer1-packet-*.json")
	} else {
		path := filepath.Join(directory, name)
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	}
	if err != nil {
		return "", err
	}
	path := file.Name()
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if temporary {
		err = file.Chmod(0600)
	}
	if err == nil {
		_, err = file.Write(encoded)
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	ok = true
	return filepath.Abs(path)
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
