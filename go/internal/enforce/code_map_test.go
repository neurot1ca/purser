package enforce

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestCodeMapNativePreBoundaryAndCompletion(t *testing.T) {
	t.Run("unwritable state is never fresh", func(t *testing.T) {
		r, s := nativeMapRepo(t, map[string]string{"one.js": "one"})
		invalidStateRoot := filepath.Join(s, "not-a-directory")
		if err := os.WriteFile(invalidStateRoot, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, status := refreshCodeMapForPreWith(r, invalidStateRoot, time.Second, nil, nil, nil); status == "FRESH" {
			t.Fatal("unwritable state became fresh")
		}
	})
	t.Run("lock contention is never fresh", func(t *testing.T) {
		r, s := nativeMapRepo(t, map[string]string{"one.js": "one"})
		p, status := refreshCodeMapNative(r, s, time.Second, nil, nil)
		if status != "FRESH" {
			t.Fatalf("seed status %s", status)
		}
		unlock, err := contextio.LockFile(filepath.Join(filepath.Dir(p), "index.lock"))
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		starts := 0
		starter := func([]string, []string, string) error { starts++; return nil }
		if _, status = refreshCodeMapForPreWith(r, s, 25*time.Millisecond, nil, nil, starter); status == "FRESH" {
			t.Fatal("lock contention became fresh")
		}
		if starts != 1 {
			t.Fatalf("stale map under lock contention started %d completions", starts)
		}
	})
	t.Run("expired pre budget keeps the published map stale and starts completion", func(t *testing.T) {
		r, s := nativeMapRepo(t, map[string]string{"one.js": "one"})
		p, status := refreshCodeMapNative(r, s, time.Second, nil, nil)
		if status != "FRESH" {
			t.Fatalf("seed status %s", status)
		}
		starts := 0
		starter := func([]string, []string, string) error { starts++; return nil }
		if got, status := refreshCodeMapForPreWith(r, s, 0, nil, nil, starter); status != "STALE" || got != p {
			t.Fatalf("expired pre status=%s path=%q want stale %q", status, got, p)
		}
		if starts != 1 {
			t.Fatalf("completion starts=%d, want 1", starts)
		}
	})
	t.Run("malformed and oversized output are rejected", func(t *testing.T) {
		for _, output := range [][]byte{[]byte("CODE_MAP=x STATUS=FRESH\nextra"), make([]byte, 4097)} {
			if _, status, err := parseCodeMapLine(output); err == nil || status == "FRESH" {
				t.Fatalf("invalid output became %q (%v)", status, err)
			}
		}
	})
	t.Run("pending starts one detached completion", func(t *testing.T) {
		r, s := nativeMapRepo(t, map[string]string{"a.js": "a", "b.js": "b", "c.js": "c"})
		calls := 0
		extract := map[string]codeMapSymbolExtractor{"javascript": func(src []byte) ([]codeMapSymbol, error) {
			time.Sleep(300 * time.Millisecond)
			return nil, nil
		}}
		starter := func(args, _ []string, dir string) error {
			calls++
			if len(args) < 2 || args[1] != "code-map" || dir != r {
				t.Fatalf("unexpected detached command args=%q dir=%q", args, dir)
			}
			return nil
		}
		if _, status := refreshCodeMapForPreWith(r, s, 250*time.Millisecond, extract, nil, starter); status != "STALE" {
			t.Fatalf("pending status %s", status)
		}
		if _, status := refreshCodeMapForPreWith(r, s, 250*time.Millisecond, extract, nil, starter); status != "STALE" {
			t.Fatalf("repeated pending status %s", status)
		}
		if calls != 1 {
			t.Fatalf("detached completions=%d, want 1", calls)
		}
	})
	t.Run("fresh starts no completion and completion resolves stale", func(t *testing.T) {
		r, s := nativeMapRepo(t, map[string]string{"a.js": "a", "b.js": "b", "c.js": "c"})
		calls := 0
		starter := func([]string, []string, string) error { calls++; return nil }
		if _, status := refreshCodeMapForPreWith(r, s, time.Second, nil, nil, starter); status != "FRESH" || calls != 0 {
			t.Fatalf("fresh status=%s starts=%d", status, calls)
		}
		if err := os.WriteFile(filepath.Join(r, "a.js"), []byte("changed"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r, "d.js"), []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
		delayed := map[string]codeMapSymbolExtractor{"javascript": func([]byte) ([]codeMapSymbol, error) { time.Sleep(300 * time.Millisecond); return nil, nil }}
		if _, status := refreshCodeMapForPreWith(r, s, 250*time.Millisecond, delayed, nil, starter); status != "STALE" {
			t.Fatalf("pending status %s", status)
		}
		if _, status := RunCodeMapCompletion(r, s, time.Second); status != "FRESH" {
			t.Fatalf("completion status %s", status)
		}
	})
}

func TestCodeMapNativeFitsOversizedRender(t *testing.T) {
	entries := make(map[string]codeMapNativeEntry, 5000)
	for i := 0; i < 5000; i++ {
		path := fmt.Sprintf("src/file-%04d.go", i)
		entries[path] = codeMapNativeEntry{MTimeNS: int64(i), Size: 1, Language: "go", Coverage: "native", Symbols: [][]any{{i + 1, "function", fmt.Sprintf("Function%04d", i), "(argument string)"}}}
	}
	data, err := renderCodeMapNativeWithPriority(entries, map[string]bool{"src/file-4999.go": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > codeMapMaxBytes {
		t.Fatalf("rendered %d bytes, limit %d", len(data), codeMapMaxBytes)
	}
	if !strings.Contains(string(data), "files omitted:") {
		t.Fatal("oversized render did not mark omitted files")
	}
	if !strings.Contains(string(data), "src/file-4999.go") {
		t.Fatal("changed file was not prioritized")
	}
}

func TestCodeMapP43Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "code_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Version int              `json:"version"`
		Parser  string           `json:"parser"`
		Limits  map[string]int64 `json:"limits"`
		Cases   []struct {
			Name              string `json:"name"`
			Op                string `json:"op"`
			Kind              string `json:"kind"`
			Version           int    `json:"version"`
			Parser            string `json:"parser"`
			Size              int64  `json:"size"`
			Total             int64  `json:"total"`
			Count             int    `json:"count"`
			ExpectedEntry     string `json:"expected_entry"`
			ExpectedRegular   bool   `json:"expected_regular"`
			ExpectedContained bool   `json:"expected_contained"`
			ExpectedValid     bool   `json:"expected_valid"`
			ExpectedHit       bool   `json:"expected_hit"`
			ExpectedSkip      bool   `json:"expected_skip"`
			ExpectedError     string `json:"expected_error"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Version != codeMapVersion || f.Limits["max_files"] != codeMapMaxFiles || f.Limits["max_file_bytes"] != codeMapMaxFileBytes || f.Limits["max_total_bytes"] != codeMapMaxTotal || f.Limits["max_map_bytes"] != codeMapMaxBytes || f.Limits["max_seconds"] != int64(codeMapMaxSeconds/time.Second) {
		t.Fatalf("fixture constants differ: %#v", f)
	}
	for _, c := range f.Cases {
		if !strings.Contains(c.Name, "p43") {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			switch c.Op {
			case "inventory":
				root := t.TempDir()
				git(t, root, "init", "-q")
				path := filepath.Join(root, "alias.txt")
				switch c.Kind {
				case "symlink-internal":
					if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("inside"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("symlink target placeholder"), 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink-escape":
					outside := filepath.Join(t.TempDir(), "outside.txt")
					if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(root, "escape.txt")
					if err := os.WriteFile(path, []byte("symlink target placeholder"), 0600); err != nil {
						t.Fatal(err)
					}
				case "tracked-directory":
					path = filepath.Join(root, "directory")
					if err := os.WriteFile(path, []byte("tracked"), 0600); err != nil {
						t.Fatal(err)
					}
					git(t, root, "add", "directory")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if c.Kind != "tracked-directory" {
					git(t, root, "add", "-f", "--", filepath.Base(path))
				}
				var state *codeMapFile
				var err error
				if c.Kind == "tracked-directory" {
					state, err = codeMapFileState(root, filepath.Base(path))
				} else {
					info, statErr := os.Lstat(path)
					if statErr != nil {
						t.Fatal(statErr)
					}
					state, err = codeMapFileStateFromInfo(root, filepath.Base(path), modeFileInfo{FileInfo: info, mode: fs.ModeSymlink})
				}
				if err != nil {
					t.Fatal(err)
				}
				if state == nil || state.Regular != c.ExpectedRegular || state.Contained != c.ExpectedContained {
					t.Fatalf("state %#v", state)
				}
				paths, err := codeMapInventory(root, time.Now().Add(time.Second))
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, item := range paths {
					if item == filepath.Base(path) {
						found = true
					}
				}
				if !found {
					t.Fatalf("path omitted from inventory: %v", paths)
				}
				if c.Kind == "tracked-directory" {
					files, skipped, scanErr := codeMapScanInventory(root, time.Now().Add(time.Second))
					if scanErr != nil || len(files) != 1 || skipped != 0 {
						t.Fatalf("scan %#v skipped=%d err=%v", files, skipped, scanErr)
					}
				}
			case "cache":
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "map.txt"), []byte("# map\n"), 0600); err != nil {
					t.Fatal(err)
				}
				manifest, _ := json.Marshal(map[string]any{"version": c.Version, "parser": c.Parser, "entries": map[string]any{}})
				if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0600); err != nil {
					t.Fatal(err)
				}
				loaded, valid, err := readCodeMapCache(dir, f.Parser, time.Now().Add(time.Second))
				if valid != c.ExpectedValid || err == nil || loaded != nil {
					t.Fatalf("cache loaded=%v valid=%v err=%v", loaded, valid, err)
				}
			case "cache-key":
				path := filepath.Join(t.TempDir(), "source")
				if err := os.WriteFile(path, make([]byte, c.Size), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, time.Unix(1, 100), time.Unix(1, 100)); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				key := codeMapCacheKey(info)
				cached := codeMapCacheEntry{MTimeNS: key.MTimeNS, Size: 9}
				if (key == cached) != c.ExpectedHit {
					t.Fatalf("cache key %#v hit=%v", key, key == cached)
				}
			case "limit":
				skipped, err := codeMapWithinLimits(c.Size, c.Total)
				if c.ExpectedError != "" {
					if err == nil || err.Error() != c.ExpectedError {
						t.Fatalf("error %v", err)
					}
					return
				}
				if err != nil || skipped != c.ExpectedSkip {
					t.Fatalf("skip=%v err=%v", skipped, err)
				}
			case "file-count":
				root := t.TempDir()
				git(t, root, "init", "-q")
				for i := 0; i < c.Count; i++ {
					if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%04d.txt", i)), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if _, _, err := codeMapScanInventory(root, time.Now().Add(codeMapMaxSeconds)); err == nil || err.Error() != c.ExpectedError {
					t.Fatalf("count error: %v", err)
				}
			}
		})
	}
}

type modeFileInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (i modeFileInfo) Mode() fs.FileMode { return i.mode }

func TestCodeMapP43CacheFailuresAreNeverFresh(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "map.txt"), []byte("map"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"version": codeMapVersion, "parser": "unavailable", "entries": map[string]any{}})
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(25 * time.Millisecond)
	if _, valid, err := readCodeMapCache(dir, "unavailable", deadline); err != nil || !valid {
		t.Fatalf("valid cache: valid=%v err=%v", valid, err)
	}
	if _, valid, err := readCodeMapCache(dir, "unavailable", time.Now().Add(-time.Second)); err == nil || valid {
		t.Fatalf("deadline reported fresh: valid=%v err=%v", valid, err)
	}
	if err := os.WriteFile(manifestPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, valid, err := readCodeMapCache(dir, "unavailable", time.Now().Add(time.Second)); err == nil || valid {
		t.Fatalf("unreadable cache reported fresh: valid=%v err=%v", valid, err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := contextio.LockFile(filepath.Join(dir, "index.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, valid, err := readCodeMapCache(dir, "unavailable", time.Now().Add(25*time.Millisecond)); err == nil || valid {
		t.Fatalf("locked cache reported fresh: valid=%v err=%v", valid, err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
