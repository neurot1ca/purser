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
			t.Fatalf("case missing p43: %s", c.Name)
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
