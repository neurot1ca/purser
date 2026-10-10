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

func TestCodeMapGoExtractor(t *testing.T) {
	source := []byte("package sample\n\ntype Widget struct { Value int }\n\nfunc NewWidget(value int) *Widget { return &Widget{Value: value} }\n\nfunc (w *Widget) Set(value int) error { w.Value = value; return nil }\n")
	symbols, err := extractCodeMapGo(source)
	if err != nil {
		t.Fatal(err)
	}
	want := []codeMapSymbol{
		{Line: 3, Kind: "type", Name: "Widget", Signature: "type Widget struct{ Value int }"},
		{Line: 5, Kind: "function", Name: "NewWidget", Signature: "func NewWidget(value int) *Widget"},
		{Line: 7, Kind: "method", Name: "Widget.Set", Signature: "func (w *Widget) Set(value int) error"},
	}
	if fmt.Sprint(symbols) != fmt.Sprint(want) {
		t.Fatalf("symbols=%#v want=%#v", symbols, want)
	}
}

func TestCodeMapGoRenderedFunctionSeparatesNameAndSignature(t *testing.T) {
	data, err := renderCodeMapNative(map[string]codeMapNativeEntry{
		"hide.go": {Language: "go", Coverage: "native", Symbols: [][]any{{12, "function", "Hide", "func Hide(command *exec.Cmd) *exec.Cmd"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "  12 function Hide(command *exec.Cmd) *exec.Cmd") {
		t.Fatalf("rendered map does not separate function name and signature:\n%s", data)
	}
}

func TestCodeMapPass3dOracleSymbols(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "code_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name            string             `json:"name"`
			Sources         map[string]string  `json:"sources"`
			ExpectedSymbols map[string][][]any `json:"expected_symbols"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	languages := map[string]string{
		"p3d_oracle_rust": "rust", "p3d_oracle_java": "java", "p3d_oracle_c": "c",
		"p3d_oracle_cpp": "cpp", "p3d_oracle_ruby": "ruby",
	}
	for _, item := range fixture.Cases {
		language, ok := languages[item.Name]
		if !ok {
			continue
		}
		t.Run(language, func(t *testing.T) {
			if len(item.Sources) != 1 {
				t.Fatalf("oracle case must contain one source: %#v", item.Sources)
			}
			for path, source := range item.Sources {
				got, err := extractTreeSitterSymbols([]byte(source), language)
				if err != nil {
					t.Fatal(err)
				}
				var want []codeMapSymbol
				for _, symbol := range item.ExpectedSymbols[path] {
					if len(symbol) != 4 {
						t.Fatalf("invalid oracle symbol: %#v", symbol)
					}
					line, ok := symbol[0].(float64)
					if !ok {
						t.Fatalf("invalid oracle line: %#v", symbol[0])
					}
					kind, _ := symbol[1].(string)
					name, _ := symbol[2].(string)
					signature, _ := symbol[3].(string)
					want = append(want, codeMapSymbol{Line: int(line), Kind: kind, Name: name, Signature: signature})
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("symbols=%#v want oracle=%#v", got, want)
				}
			}
		})
	}
}

func TestCodeMapPass3eSymbols(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "code_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name            string             `json:"name"`
			Sources         map[string]string  `json:"sources"`
			ExpectedSymbols map[string][][]any `json:"expected_symbols"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	languages := map[string]string{"php": "php", "swift": "swift", "kotlin": "kotlin", "scala": "scala", "lua": "lua", "bash": "bash", "go_only_c_sharp": "c_sharp"}
	for _, item := range fixture.Cases {
		if !strings.HasPrefix(item.Name, "p3e_") {
			continue
		}
		language := ""
		for suffix, value := range languages {
			if strings.HasSuffix(item.Name, suffix) {
				language = value
				break
			}
		}
		if language == "" {
			t.Fatalf("unclassified p3e case %s", item.Name)
		}
		t.Run(item.Name, func(t *testing.T) {
			for path, source := range item.Sources {
				got, err := extractTreeSitterSymbols([]byte(source), language)
				if err != nil {
					t.Fatal(err)
				}
				var want []codeMapSymbol
				for _, symbol := range item.ExpectedSymbols[path] {
					line, _ := symbol[0].(float64)
					kind, _ := symbol[1].(string)
					name, _ := symbol[2].(string)
					signature, _ := symbol[3].(string)
					want = append(want, codeMapSymbol{Line: int(line), Kind: kind, Name: name, Signature: signature})
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("symbols=%#v want=%#v", got, want)
				}
			}
		})
	}
}

func TestCodeMapNativeResumesPendingExtraction(t *testing.T) {
	r, s := nativeMapRepo(t, map[string]string{"a.js": "a", "b.js": "b", "c.js": "c"})
	calls := map[string]int{}
	extract := map[string]codeMapSymbolExtractor{"javascript": func(src []byte) ([]codeMapSymbol, error) {
		calls[string(src)]++
		if string(src) == "a" {
			time.Sleep(1100 * time.Millisecond)
		}
		return []codeMapSymbol{{Line: 1, Kind: "function", Name: string(src)}}, nil
	}}
	p, status := refreshCodeMapNative(r, s, time.Second, extract, nil)
	first, err := os.ReadFile(p)
	if err != nil || status != "STALE" || calls["a"] != 1 || calls["b"] != 0 || calls["c"] != 0 || !strings.Contains(string(first), "b.js\n  symbols: pending") || !strings.Contains(string(first), "c.js\n  symbols: pending") {
		t.Fatalf("first status=%s err=%v calls=%v\n%s", status, err, calls, first)
	}
	_, status = refreshCodeMapNative(r, s, time.Second, extract, nil)
	second, err := os.ReadFile(p)
	if err != nil || status != "FRESH" || calls["a"] != 1 || calls["b"] != 1 || calls["c"] != 1 || strings.Contains(string(second), "symbols: pending") {
		t.Fatalf("second status=%s err=%v calls=%v\n%s", status, err, calls, second)
	}
}

func TestCodeMapNativeDeadlineBetweenFilesKeepsCompletedEntries(t *testing.T) {
	r, s := nativeMapRepo(t, map[string]string{"a.js": "a", "b.js": "b", "c.js": "c"})
	calls := map[string]int{}
	extract := map[string]codeMapSymbolExtractor{"javascript": func(src []byte) ([]codeMapSymbol, error) {
		calls[string(src)]++
		if string(src) == "b" {
			time.Sleep(1100 * time.Millisecond)
		}
		return []codeMapSymbol{{Line: 1, Kind: "function", Name: string(src)}}, nil
	}}
	p, status := refreshCodeMapNative(r, s, time.Second, extract, nil)
	got, err := os.ReadFile(p)
	text := string(got)
	if err != nil || status != "STALE" || calls["a"] != 1 || calls["b"] != 1 || calls["c"] != 0 || !strings.Contains(text, "a.js\n  1 function a") || !strings.Contains(text, "c.js\n  symbols: pending") {
		t.Fatalf("status=%s err=%v calls=%v\n%s", status, err, calls, got)
	}
	if strings.Index(text, "a.js") > strings.Index(text, "c.js") {
		t.Fatalf("completed entry was not rendered before pending entry:\n%s", got)
	}
}

func TestCodeMapNativeDefaultExtractors(t *testing.T) {
	r, s := nativeMapRepo(t, map[string]string{
		"sample.py":  "class Box:\n    def open(self):\n        pass\n",
		"sample.js":  "export default () => {};\nexport const run = () => {};\nclass Box { open() {} }\n",
		"sample.ts":  "export default () => {};\nexport const run = () => {};\nclass Box { open() {} }\n",
		"sample.tsx": "export default () => {};\nexport const run = () => {};\nclass Box { open() {} }\n",
		"sample.rs":  "fn unavailable() {}\n",
	})
	p, status := refreshCodeMapNative(r, s, time.Second, nil, nil)
	got, err := os.ReadFile(p)
	text := string(got)
	if err != nil || status != "FRESH" {
		t.Fatalf("status=%s err=%v\n%s", status, err, got)
	}
	for _, want := range []string{
		"Parser: native Go/Python/tree-sitter", "sample.py\n  1 class Box\n  2 method Box.open",
		"sample.js\n  1 function default\n  2 function run\n  3 class Box\n  3 method Box.open",
		"sample.ts\n  1 function default\n  2 function run\n  3 class Box\n  3 method Box.open",
		"sample.tsx\n  1 function default\n  2 function run\n  3 class Box\n  3 method Box.open",
		"sample.rs\n  1 function unavailable",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in map:\n%s", want, text)
		}
	}
	if strings.Contains(text, "tree-sitter-language-pack") || strings.Contains(text, "Python ast;") {
		t.Fatalf("obsolete parser guidance remains:\n%s", text)
	}
}

func TestCodeMapNativeOracleOrderingAndTruncation(t *testing.T) {
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
	text := string(got)
	if err != nil || status != "FRESH" || !strings.HasPrefix(text, "# Code map v5\nParser: native Go/Python/tree-sitter\n") ||
		!strings.Contains(text, "a_first.py\n  1 function first(argument_0: str='value'") ||
		!strings.Contains(text, "m_middle.py\n  1 class Zebra\n  3 class Alpha") ||
		!strings.Contains(text, "z_last.py\n  1 function zed()") {
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
	p := filepath.Join("testdata", "code_map.json")
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
