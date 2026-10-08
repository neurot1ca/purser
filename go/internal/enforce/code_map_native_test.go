package enforce

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestCodeMapNativeOracleUnavailable(t *testing.T) {
	w := nativeMapFixture(t, "p43_oracle_unavailable")
	r, s := nativeMapRepo(t, map[string]string{"sample.js": "function hello() {}\n"})
	p, status := refreshCodeMapNative(r, s, time.Second, nil, nil)
	got, err := os.ReadFile(p)
	if err != nil || status != "FRESH" || string(got) != w {
		t.Fatalf("status=%s err=%v\n%s", status, err, got)
	}
}

func TestCodeMapNativeOracleOrderingAndTruncation(t *testing.T) {
	w := nativeMapFixture(t, "p43_oracle_render")
	r, s := nativeMapRepo(t, map[string]string{"z_last.py": "z", "m_middle.py": "m", "a_first.py": "a"})
	e := map[string]codeMapSymbolExtractor{"python": func(src []byte) ([]codeMapSymbol, error) {
		if string(src) == "a" {
			a := make([]string, 35)
			for i := range a {
				a[i] = fmt.Sprintf("argument_%d: str='value'", i)
			}
			return []codeMapSymbol{{Line: 1, Kind: "function", Name: "first", Signature: "(" + strings.Join(a, ", ") + ") -> dict[str, str]"}}, nil
		}
		if string(src) == "m" {
			return []codeMapSymbol{{Line: 1, Kind: "class", Name: "Zebra"}, {Line: 3, Kind: "class", Name: "Alpha"}}, nil
		}
		return []codeMapSymbol{{Line: 1, Kind: "function", Name: "zed", Signature: "()"}}, nil
	}}
	p, status := refreshCodeMapNative(r, s, time.Second, e, nil)
	got, err := os.ReadFile(p)
	if err != nil || status != "FRESH" || string(got) != w {
		t.Fatalf("status=%s err=%v\n%s", status, err, got)
	}
}

func TestCodeMapNativeReusesCacheAndDropsDeletedFiles(t *testing.T) {
	r, s := nativeMapRepo(t, map[string]string{"one.js": "one", "two.js": "two"})
	calls := 0
	e := map[string]codeMapSymbolExtractor{"javascript": func([]byte) ([]codeMapSymbol, error) { calls++; return nil, nil }}
	p, status := refreshCodeMapNative(r, s, time.Second, e, nil)
	if status != "FRESH" || calls != 2 {
		t.Fatalf("initial %s calls=%d", status, calls)
	}
	if _, status = refreshCodeMapNative(r, s, time.Second, e, nil); status != "FRESH" || calls != 2 {
		t.Fatalf("cache %s calls=%d", status, calls)
	}
	if err := os.Remove(filepath.Join(r, "one.js")); err != nil {
		t.Fatal(err)
	}
	_, status = refreshCodeMapNative(r, s, time.Second, e, nil)
	data, err := os.ReadFile(p)
	if err != nil || status != "FRESH" || strings.Contains(string(data), "one.js") || !strings.Contains(string(data), "two.js") {
		t.Fatalf("delete %s %v\n%s", status, err, data)
	}
}

func TestCodeMapNativeFailuresPreserveMapAndNeverFresh(t *testing.T) {
	for _, f := range []string{"lock", "write", "limit"} {
		t.Run(f, func(t *testing.T) {
			r, s := nativeMapRepo(t, map[string]string{"keep.js": "ok"})
			p, status := refreshCodeMapNative(r, s, time.Second, nil, nil)
			if status != "FRESH" {
				t.Fatalf("seed %s", status)
			}
			before, _ := os.ReadFile(p)
			budget := time.Second
			if f == "lock" {
				budget = 35 * time.Millisecond
				unlock, err := contextio.LockFile(filepath.Join(filepath.Dir(p), "index.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			if f == "write" {
				if err := os.WriteFile(filepath.Join(r, "keep.js"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if f == "limit" {
				name := filepath.Join(r, "bulk.bin")
				if err := os.WriteFile(name, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(name, codeMapMaxTotal+1); err != nil {
					t.Fatal(err)
				}
				git(t, r, "add", "bulk.bin")
			}
			var writer func(string, []byte) error
			if f == "write" {
				writer = func(string, []byte) error { return os.ErrPermission }
			}
			_, status = refreshCodeMapNative(r, s, budget, nil, writer)
			after, err := os.ReadFile(p)
			if status == "FRESH" || err != nil || string(after) != string(before) {
				t.Fatalf("status=%s err=%v changed=%v", status, err, string(after) != string(before))
			}
		})
	}
}

func nativeMapRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	r := t.TempDir()
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(r, n), []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, r, "init", "-q")
	git(t, r, "add", ".")
	return r, t.TempDir()
}

func nativeMapFixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "code_map.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name string
			Map  string `json:"expected_map"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		if c.Name == name {
			return c.Map
		}
	}
	t.Fatalf("missing fixture %s", name)
	return ""
}
