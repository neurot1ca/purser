package measures

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"purser-hook/internal/obsidian"
	"purser-hook/internal/profile"
)

type Item struct {
	Title, Status, Due, Text string
	Paths                    []string
}

var (
	resultPattern = regexp.MustCompile(`(?i)\bResult\s*:`)
	duePattern    = regexp.MustCompile(`(?i)\bDue\s*:?\s*([0-9]{4}-[0-9]{2}-[0-9]{2})\b`)
	pathPattern   = regexp.MustCompile(`(?:^|[\s(])([A-Za-z0-9_./-]+\.(?:md|jsonl|json|csv))(?:$|[\s),;.])`)
	markerPattern = regexp.MustCompile(`(?m)^<!-- orchestrator projection v1 sha256=([0-9a-f]{64}) -->\n`)
)

func Parse(data []byte, section string, exists func(string) bool) []Item {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if strings.TrimSpace(section) != "" {
		want := strings.TrimSpace(section)
		if strings.HasPrefix(want, "#") {
			want = strings.TrimSpace(strings.TrimLeft(want, "#"))
		}
		start := -1
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") && strings.TrimSpace(strings.TrimLeft(trimmed, "#")) == want {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil
		}
		end := len(lines)
		for i := start; i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			if strings.HasPrefix(trimmed, "#") {
				end = i
				break
			}
		}
		lines = lines[start:end]
	}
	var items []Item
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], "- ") {
			i++
			continue
		}
		start := i
		i++
		for i < len(lines) && !strings.HasPrefix(lines[i], "- ") {
			i++
		}
		text := strings.TrimSpace(strings.Join(lines[start:i], "\n"))
		lead := strings.SplitN(strings.TrimPrefix(text, "- "), "**", 3)
		if len(lead) < 3 {
			continue
		}
		title := strings.TrimSuffix(strings.TrimSpace(lead[1]), ".")
		if title == "" {
			continue
		}
		item := Item{Title: title, Text: text, Status: "done"}
		if strings.Contains(strings.ToLower(text), "result: pending") || !resultPattern.MatchString(text) {
			item.Status = "pending"
		}
		if match := duePattern.FindStringSubmatch(text); len(match) > 1 {
			item.Due = match[1]
		}
		seen := map[string]bool{}
		for _, match := range pathPattern.FindAllStringSubmatch(text, -1) {
			path := filepath.Clean(filepath.FromSlash(match[1]))
			if filepath.IsAbs(path) || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) || seen[path] {
				continue
			}
			if exists(filepath.ToSlash(path)) {
				item.Paths = append(item.Paths, filepath.ToSlash(path))
				seen[path] = true
			}
		}
		items = append(items, item)
	}
	return items
}

// Sync reads a project's configured ledger and projects it into the enabled vault.
// The returned count is the number of item notes written.
func Sync(ctx context.Context, repoRoot string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return 0, err
	}
	resolved := profile.LoadProfile(repoRoot)
	ledgerSpec := "NEXT.md#Measurements ledger"
	if resolved.MeasurementLedger != nil && strings.TrimSpace(*resolved.MeasurementLedger) != "" {
		ledgerSpec = *resolved.MeasurementLedger
	}
	file, section := splitSpec(ledgerSpec)
	ledgerPath := filepath.Join(repoRoot, filepath.FromSlash(file))
	if !within(repoRoot, ledgerPath) {
		return 0, nil
	}
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		return 0, nil
	}
	items := Parse(data, section, func(path string) bool {
		return within(repoRoot, filepath.Join(repoRoot, filepath.FromSlash(path))) && regular(filepath.Join(repoRoot, filepath.FromSlash(path)))
	})
	if len(items) == 0 {
		return 0, nil
	}
	config, _ := os.ReadFile(obsidian.ConfigPath())
	envVault, hasEnv := os.LookupEnv("PURSER_OBSIDIAN_VAULT")
	var env *string
	if hasEnv {
		env = &envVault
	}
	enabled, vault := obsidian.ProjectionSettings(config, env, nil)
	if !enabled || vault == "" {
		return 0, nil
	}
	key := projectFolder(vault, repoRoot)
	dir := filepath.Join(vault, "Measurements", key)
	written := 0
	sections := make([]string, 0, len(items))
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		slug := slug(item.Title)
		if slug == "" {
			slug = fmt.Sprintf("item-%d", index+1)
		}
		note := renderItem(item, key, file)
		notePath := filepath.Join(dir, slug+".md")
		changed, writeErr := false, error(nil)
		if within(vault, notePath) {
			changed, writeErr = writeProjected(notePath, note)
		}
		if writeErr == nil && changed {
			written++
		}
		links := []string{}
		for _, path := range item.Paths {
			if strings.EqualFold(filepath.Ext(path), ".md") {
				targetName := strings.ReplaceAll(strings.ReplaceAll(path, "/", "-"), "\\", "-")
				copyPath := filepath.Join(dir, "Reports", targetName)
				sourcePath := filepath.Join(repoRoot, filepath.FromSlash(path))
				raw, readErr := os.ReadFile(sourcePath)
				if readErr == nil && within(vault, copyPath) && within(repoRoot, sourcePath) {
					_, _ = writeProjected(copyPath, marker(string(raw)))
				}
				links = append(links, "[[Reports/"+strings.TrimSuffix(targetName, ".md")+"]]")
			} else {
				absolute := filepath.Join(repoRoot, filepath.FromSlash(path))
				if within(repoRoot, absolute) {
					links = append(links, absolute)
				}
			}
		}
		sections = append(sections, fmt.Sprintf("## %s\n\n- Status: %s\n- Due: %s\n\n%s\n", item.Title, item.Status, display(item.Due), strings.Join(links, "  \n")))
	}
	if err := ctx.Err(); err != nil {
		return written, err
	}
	ledgerOut := filepath.Join(dir, "Ledger.md")
	if within(vault, ledgerOut) {
		_, _ = writeProjected(ledgerOut, marker(fmt.Sprintf("source: %q\n\n# Measurements\n\n%s", repoRoot, strings.Join(sections, "\n"))))
	}
	base := fmt.Sprintf("filters:\n  and:\n    - file.inFolder(\"Measurements/%s\")\nviews:\n  - type: table\n    name: Measurements\n    order:\n      - file.name\n      - status\n      - due\n    sort:\n      - property: status\n        direction: ASC\n      - property: due\n        direction: ASC\n", key)
	viewOut := filepath.Join(vault, "Views", "Measurements.base")
	if within(vault, viewOut) {
		_, _ = writeProjected(viewOut, marker(base))
	}
	return written, nil
}

func splitSpec(value string) (string, string) {
	parts := strings.SplitN(value, "#", 2)
	file := strings.TrimSpace(parts[0])
	section := ""
	if len(parts) == 2 {
		section = strings.TrimSpace(parts[1])
	}
	return file, section
}
func regular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
func within(root, path string) bool {
	r, e1 := resolvedPath(root)
	p, e2 := resolvedPath(path)
	if e1 != nil || e2 != nil {
		return false
	}
	rel, err := filepath.Rel(r, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := abs
	suffix := []string{}
	for {
		real, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return filepath.Clean(real), nil
		}
		if !os.IsNotExist(evalErr) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(abs), nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
func projectKey(root string) string {
	value := root
	if runtime.GOOS == "windows" {
		value = strings.ToLower(value)
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:16]
}
func projectFolder(vault, repoRoot string) string {
	name := slug(filepath.Base(filepath.Clean(repoRoot)))
	if name == "" {
		name = "project"
	}
	base := filepath.Join(vault, "Measurements", name, "Ledger.md")
	if source, ok := projectedSource(base); !ok || samePath(source, repoRoot) {
		return name
	}
	return name + "-" + projectKey(repoRoot)
}
func projectedSource(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || !intact(string(data)) {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "source: ") {
			value, err := strconv.Unquote(strings.TrimPrefix(line, "source: "))
			return value, err == nil && value != ""
		}
	}
	return "", false
}
func samePath(left, right string) bool {
	a, errA := filepath.Abs(left)
	b, errB := filepath.Abs(right)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
func slug(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	dash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
func display(value string) string {
	if value == "" {
		return "unspecified"
	}
	return value
}
func renderItem(item Item, key, source string) string {
	due := item.Due
	if due == "" {
		due = "null"
	}
	body := fmt.Sprintf("---\ntitle: %q\nstatus: %q\ndue: %s\nproject: %q\nsource: %q\n---\n\n# %s\n\n%s\n", item.Title, item.Status, due, key, source, item.Title, item.Text)
	return marker(body)
}
func marker(content string) string {
	digest := sha256.Sum256([]byte(content))
	return "<!-- orchestrator projection v1 sha256=" + hex.EncodeToString(digest[:]) + " -->\n" + content
}
func intact(content string) bool {
	match := markerPattern.FindStringSubmatchIndex(content)
	if match == nil {
		return false
	}
	without := content[:match[0]] + content[match[1]:]
	digest := sha256.Sum256([]byte(without))
	return hex.EncodeToString(digest[:]) == content[match[2]:match[3]]
}
func writeProjected(path, content string) (bool, error) {
	if data, err := os.ReadFile(path); err == nil {
		if string(data) == content {
			return false, nil
		}
		if !intact(string(data)) {
			return false, nil
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".measure-*.tmp")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err = tmp.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(name, path); err != nil {
		return false, err
	}
	return true, nil
}

// CurrentRepo finds the enclosing Git worktree from the current directory.
func CurrentRepo() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
