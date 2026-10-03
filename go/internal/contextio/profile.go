package contextio

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// profileProtectedPaths reads only schema_version and [paths].protected.
// Unsupported or malformed tiers fail closed and contribute no extra paths.
func profileProtectedPaths(project string) []string {
	all := append([]string(nil), protected...)
	if home, err := os.UserHomeDir(); err == nil {
		if extra, ok := readProtectedProfile(filepath.Join(home, ".orchestrator", "config.toml")); ok {
			all = append(all, extra...)
		}
	}
	if project != "" {
		if extra, ok := readProtectedProfile(filepath.Join(project, ".orchestrator", "profile.toml")); ok {
			all = append(all, extra...)
		}
	}
	set := make(map[string]bool, len(all))
	for _, p := range all {
		set[p] = true
	}
	all = all[:0]
	for p := range set {
		all = append(all, p)
	}
	sort.Strings(all)
	return all
}

func readProtectedProfile(path string) ([]string, bool) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	return parseProtectedProfile(b)
}

func parseProtectedProfile(data []byte) ([]string, bool) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	section, pending := "", ""
	arrayTables := map[string]int{}
	seen := map[string]bool{}
	version, versionSeen := false, false
	var paths []string
	protectedSeen := false
	for scanner.Scan() {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if line == "" {
			continue
		}
		if pending != "" {
			pending += " " + line
			if !balancedTOMLValue(strings.SplitN(pending, "=", 2)[1]) {
				continue
			}
			line, pending = pending, ""
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, false
			}
			arrayTable := strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]")
			start, end := 1, len(line)-1
			if arrayTable {
				start, end = 2, len(line)-2
			}
			name := strings.TrimSpace(line[start:end])
			if name == "" || strings.ContainsAny(name, "[]") || (arrayTable && strings.HasPrefix(name, "paths")) {
				return nil, false
			}
			if arrayTable {
				arrayTables[name]++
				section = name + "#" + strconv.Itoa(arrayTables[name])
				continue
			}
			if name == "paths" {
				if seen["table:paths"] {
					return nil, false
				}
				seen["table:paths"] = true
			} else if strings.HasPrefix(name, "paths.") {
				return nil, false
			}
			section = name
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, false
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" || value == "" || strings.ContainsAny(key, " \t") {
			return nil, false
		}
		if !balancedTOMLValue(value) {
			pending = line
			continue
		}
		full := key
		if section != "" {
			full = section + "." + key
		}
		if seen[full] {
			return nil, false
		}
		seen[full] = true
		switch full {
		case "schema_version":
			n, err := strconv.ParseInt(strings.ReplaceAll(value, "_", ""), 0, 64)
			if err != nil || n != 1 || (len(value) > 1 && value[0] == '0') {
				return nil, false
			}
			version, versionSeen = true, true
		case "paths.protected":
			var valid bool
			paths, valid = parseStringArray(value)
			if !valid {
				return nil, false
			}
			protectedSeen = true
		case "paths":
			return nil, false
		default:
			if section == "paths" || strings.HasPrefix(full, "paths.") {
				return nil, false
			}
			if !validOpaqueTOMLValue(value) {
				return nil, false
			}
		}
	}
	if scanner.Err() != nil || pending != "" || !versionSeen || !version {
		return nil, false
	}
	if !protectedSeen {
		return nil, true
	}
	return paths, true
}

func stripTOMLComment(line string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if quote == '"' && c == '\\' && !escaped {
				escaped = true
				continue
			}
			if c == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '#' {
			return line[:i]
		}
	}
	return line
}

func balancedTOMLValue(s string) bool {
	var quote byte
	escaped := false
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if quote == '"' && c == '\\' && !escaped {
				escaped = true
				continue
			}
			if c == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '[' || c == '{' {
			depth++
		}
		if c == ']' || c == '}' {
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return quote == 0 && depth == 0
}

func validOpaqueTOMLValue(s string) bool {
	s = strings.TrimSpace(s)
	if s == "true" || s == "false" {
		return true
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		_, err := parseTOMLString(s)
		return err == nil
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return !strings.Contains(s[1:len(s)-1], "'")
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") || strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return balancedTOMLValue(s)
	}
	if _, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64); err == nil {
		return true
	}
	// TOML date and time values are intentionally outside this tiny reader.
	return false
}

func parseStringArray(s string) ([]string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, false
	}
	s = s[1 : len(s)-1]
	var out []string
	for len(strings.TrimSpace(s)) > 0 {
		s = strings.TrimLeft(s, " \t\r\n")
		if len(s) == 0 {
			break
		}
		quote := s[0]
		if quote != '"' && quote != '\'' {
			return nil, false
		}
		end := -1
		escaped := false
		for i := 1; i < len(s); i++ {
			if quote == '"' && s[i] == '\\' && !escaped {
				escaped = true
				continue
			}
			if s[i] == quote && !escaped {
				end = i
				break
			}
			escaped = false
		}
		if end < 0 {
			return nil, false
		}
		token := s[:end+1]
		var value string
		var err error
		if quote == '"' {
			value, err = parseTOMLString(token)
			if err != nil {
				return nil, false
			}
		} else {
			value = token[1 : len(token)-1]
		}
		out = append(out, value)
		s = strings.TrimLeft(s[end+1:], " \t\r\n")
		if len(s) > 0 && s[0] != ',' {
			return nil, false
		}
		if len(s) > 0 {
			s = s[1:]
		}
	}
	return out, true
}

func parseTOMLString(token string) (string, error) {
	for i := 1; i < len(token)-1; i++ {
		if token[i] != '\\' {
			continue
		}
		i++
		if i >= len(token)-1 {
			return "", strconv.ErrSyntax
		}
		switch token[i] {
		case '"', '\\', 'b', 't', 'n', 'f', 'r':
		case 'u':
			if i+4 >= len(token)-1 || !isHex(token[i+1:i+5]) {
				return "", strconv.ErrSyntax
			}
			i += 4
		case 'U':
			if i+8 >= len(token)-1 || !isHex(token[i+1:i+9]) {
				return "", strconv.ErrSyntax
			}
			i += 8
		default:
			return "", strconv.ErrSyntax
		}
	}
	return strconv.Unquote(token)
}
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
