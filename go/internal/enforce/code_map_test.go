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

func TestCodeMapP44Subprocess(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Fatal("Python 3.11+ required by python-locator")
	}
	if _, err = exec.LookPath("node"); err != nil {
		t.Fatal("Node required by python-locator")
	}
	pluginRoot := t.TempDir()
	if err = os.MkdirAll(filepath.Join(pluginRoot, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	locatorPath := filepath.Join(pluginRoot, "bin", "python-locator.js")
	locator := "process.stdout.write(JSON.stringify({path: process.env.P44_PYTHON}) + '\\n');\n"
	if err = os.WriteFile(locatorPath, []byte(locator), 0600); err != nil {
		t.Fatal(err)
	}
	mapDir := filepath.Join(pluginRoot, ".claude", "hooks", "orchestrator")
	if err = os.MkdirAll(mapDir, 0700); err != nil {
		t.Fatal(err)
	}
	mapScript := "import os, sys, time\n"
	mapScript += "assert sys.argv[1:] == ['--repo', os.environ['P44_REPO'], '--budget-seconds', '0.2']\n"
	mapScript += "mode = os.environ['P44_MODE']\n"
	mapScript += "if mode == 'timeout': time.sleep(5)\n"
	mapScript += "print(os.environ['P44_OUTPUT'])\n"
	mapScript += "raise SystemExit(2 if mode == 'failure' else 1 if mode == 'unavailable' else 0)\n"
	if err = os.WriteFile(filepath.Join(mapDir, "code_map.py"), []byte(mapScript), 0600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_ROOT", pluginRoot)
	t.Setenv("P44_PYTHON", python)
	t.Setenv("P44_REPO", repo)
	t.Setenv("P44_MODE", "golden")
	const mapPath = `C:\Users\test\code map\map.txt`
	for _, item := range []struct{ name, status string }{{"fresh", "FRESH"}, {"stale", "STALE"}, {"unavailable", "UNAVAILABLE"}} {
		t.Run("p44_golden_"+item.name, func(t *testing.T) {
			t.Setenv("P44_MODE", item.name)
			t.Setenv("P44_OUTPUT", "CODE_MAP="+mapPath+" STATUS="+item.status)
			gotPath, gotStatus := refreshCodeMapForPre(repo)
			if gotPath != mapPath || gotStatus != item.status {
				t.Fatalf("got %q %q", gotPath, gotStatus)
			}
		})
	}
	for _, item := range []struct{ name, output string }{
		{"failure", "CODE_MAP=" + mapPath + " STATUS=FRESH"},
		{"malformed", "not a code map line"},
	} {
		t.Run("p44_unavailable_"+item.name, func(t *testing.T) {
			t.Setenv("P44_MODE", item.name)
			t.Setenv("P44_OUTPUT", item.output)
			if path, status := refreshCodeMapForPre(repo); status != "UNAVAILABLE" || path != "" {
				t.Fatalf("failure became %q %q", path, status)
			}
		})
	}
	t.Run("p44_unavailable_timeout", func(t *testing.T) {
		t.Setenv("P44_MODE", "timeout")
		t.Setenv("P44_OUTPUT", "CODE_MAP="+mapPath+" STATUS=FRESH")
		started := time.Now()
		if path, status := refreshCodeMapForPre(repo); status != "UNAVAILABLE" || path != "" {
			t.Fatalf("timeout became %q %q", path, status)
		}
		if elapsed := time.Since(started); elapsed >= codeMapProcessLimit+time.Second {
			t.Fatalf("deadline exceeded: %s", elapsed)
		}
	})
	t.Run("p44_unavailable_missing_interpreter", func(t *testing.T) {
		if err := os.WriteFile(locatorPath, []byte("process.stdout.write('null\\n'); process.exit(1);\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if path, status := refreshCodeMapForPre(repo); status != "UNAVAILABLE" || path != "" {
			t.Fatalf("missing interpreter became %q %q", path, status)
		}
	})
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
