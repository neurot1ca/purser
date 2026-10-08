package enforce

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"purser-hook/internal/contextio"
)

type codeMapSymbol struct {
	Line                  int
	Kind, Name, Signature string
}

type codeMapSymbolExtractor func([]byte) ([]codeMapSymbol, error)

type codeMapNativeEntry struct {
	MTimeNS  int64   `json:"mtime_ns"`
	Size     int64   `json:"size"`
	Language string  `json:"language"`
	Coverage string  `json:"coverage"`
	Symbols  [][]any `json:"symbols"`
}

func refreshCodeMapNative(repo, stateRoot string, budget time.Duration, extractors map[string]codeMapSymbolExtractor, write func(string, []byte) error) (string, string) {
	repo, err := filepath.Abs(repo)
	if err != nil {
		return "", "UNAVAILABLE"
	}
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		return "", "UNAVAILABLE"
	}
	target := filepath.Join(stateRoot, "projects", contextio.ProjectKey(repo), "code-map", "map.txt")
	if write == nil {
		write = atomicWrite
	}
	deadline := time.Now().Add(min(codeMapMaxSeconds, max(0, budget)))
	fail := func() (string, string) {
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
			return target, "STALE"
		}
		return target, "UNAVAILABLE"
	}
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fail()
	}
	manifest, valid, _ := readCodeMapCache(directory, "unavailable", deadline)
	if !valid {
		manifest = nil
	}
	lock, err := os.OpenFile(filepath.Join(directory, "index.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail()
	}
	defer lock.Close()
	info, statErr := lock.Stat()
	if statErr != nil {
		return fail()
	}
	if info.Size() == 0 {
		if _, err = lock.WriteAt([]byte("0"), 0); err != nil {
			return fail()
		}
	}
	for contextio.TryLock(lock) != nil {
		if !time.Now().Before(deadline) {
			return fail()
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
	defer contextio.Unlock(lock)
	if codeMapCheckDeadline(deadline) != nil {
		return fail()
	}
	cache, readErr := os.ReadFile(filepath.Join(directory, "manifest.json"))
	var latest codeMapManifest
	if readErr == nil && json.Unmarshal(cache, &latest) == nil && latest.Version == codeMapVersion && latest.Parser == "unavailable" && latest.Entries != nil {
		if info, e := os.Stat(target); e == nil && info.Mode().IsRegular() {
			manifest = &latest
		} else {
			manifest = nil
		}
	} else {
		manifest = nil
	}
	old := map[string]json.RawMessage{}
	if manifest != nil {
		old = manifest.Entries
	}
	files, skipped, err := codeMapScanInventory(repo, deadline)
	if err != nil {
		return fail()
	}
	entries := make(map[string]codeMapNativeEntry, len(files))
	changed := false
	for _, file := range files {
		if err = codeMapCheckDeadline(deadline); err != nil {
			return fail()
		}
		key := codeMapCacheKey(file.Info)
		var prior codeMapNativeEntry
		if file.Regular && file.Contained && old[file.Path] != nil && json.Unmarshal(old[file.Path], &prior) == nil && prior.MTimeNS == key.MTimeNS && prior.Size == key.Size {
			entries[file.Path] = prior
			continue
		}
		changed = true
		entry := codeMapNativeEntry{MTimeNS: key.MTimeNS, Size: key.Size, Language: codeMapLanguage(file.Path), Coverage: "unavailable", Symbols: [][]any{}}
		if !file.Regular || !file.Contained {
			entry.Language = "unknown"
			entries[file.Path] = entry
			continue
		}
		source, readErr := os.ReadFile(filepath.Join(repo, filepath.FromSlash(file.Path)))
		if readErr != nil {
			return fail()
		}
		after, statErr := os.Stat(filepath.Join(repo, filepath.FromSlash(file.Path)))
		if statErr != nil || codeMapCacheKey(after) != key {
			return fail()
		}
		if len(source) > 0 && !utf8.Valid(source) || strings.IndexByte(string(source), 0) >= 0 {
			skipped++
			continue
		}
		if extract := extractors[entry.Language]; extract != nil {
			symbols, parseErr := extract(source)
			if parseErr == nil {
				entry.Coverage = "native"
				for _, symbol := range symbols {
					entry.Symbols = append(entry.Symbols, []any{symbol.Line, symbol.Kind, symbol.Name, symbol.Signature})
				}
			}
		}
		entries[file.Path] = entry
	}
	data, err := renderCodeMapNative(entries)
	if err != nil || codeMapCheckDeadline(deadline) != nil {
		return fail()
	}
	if len(old) != len(entries) {
		changed = true
	}
	encodedEntries := make(map[string]json.RawMessage, len(entries))
	for path, entry := range entries {
		item, _ := json.Marshal(entry)
		encodedEntries[path] = item
	}
	encoded, err := json.Marshal(codeMapManifest{Version: codeMapVersion, Parser: "unavailable", Entries: encodedEntries, SkippedFiles: skipped})
	if err != nil {
		return fail()
	}
	if manifest == nil || changed || manifest.SkippedFiles != skipped {
		if err = write(filepath.Join(directory, "manifest.json"), encoded); err != nil {
			return fail()
		}
		if err = write(target, data); err != nil {
			return fail()
		}
	}
	return target, "FRESH"
}
func codeMapLanguage(path string) string {
	language := map[string]string{".py": "python", ".pyi": "python", ".js": "javascript", ".jsx": "javascript", ".ts": "typescript", ".tsx": "tsx", ".go": "go", ".rs": "rust", ".java": "java", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp", ".cs": "c_sharp", ".rb": "ruby", ".php": "php", ".swift": "swift", ".kt": "kotlin", ".kts": "kotlin", ".scala": "scala", ".lua": "lua", ".sh": "bash", ".bash": "bash"}
	if value := language[strings.ToLower(filepath.Ext(path))]; value != "" {
		return value
	}
	return "unknown"
}
func renderCodeMapNative(entries map[string]codeMapNativeEntry) ([]byte, error) {
	coveredSet := map[string]bool{}
	for _, entry := range entries {
		if entry.Coverage != "unavailable" {
			coveredSet[entry.Language] = true
		}
	}
	covered := make([]string, 0, len(coveredSet))
	for language := range coveredSet {
		covered = append(covered, language)
	}
	sort.Strings(covered)
	if len(covered) == 0 {
		covered = []string{"none"}
	}
	lines := []string{fmt.Sprintf("# Code map v%d", codeMapVersion), "Parser: Python ast; other languages: unavailable", "Covered languages: " + strings.Join(covered, ", "), ""}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		entry := entries[path]
		lines = append(lines, path)
		if entry.Coverage == "unavailable" && entry.Language != "unknown" && entry.Language != "python" {
			lines = append(lines, "  symbols: unavailable (install tree-sitter-language-pack)")
		}
		for _, symbol := range entry.Symbols {
			if len(symbol) != 4 {
				continue
			}
			line := fmt.Sprint(symbol[0])
			kind, name, signature := fmt.Sprint(symbol[1]), fmt.Sprint(symbol[2]), fmt.Sprint(symbol[3])
			rendered := fmt.Sprintf("  %s %s %s%s", line, kind, name, signature)
			if signature != "" && utf8.RuneCountInString(rendered) > 120 {
				runes := []rune(rendered)
				rendered = string(runes[:119]) + "…"
			}
			lines = append(lines, rendered)
		}
	}
	data := []byte(strings.Join(lines, "\n") + "\n")
	if len(data) > codeMapMaxBytes {
		return nil, errors.New("rendered map")
	}
	return data, nil
}
