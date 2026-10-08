package enforce

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const pythonOracleSource = `@module_decorator
class Outer(Base[int]):
    @classmethod
    @decorate("factory")
    def create(cls, value: dict[str, int], *items: str, flag: bool = True, **kwargs: object) -> "Outer":
        def inner(item: tuple[int, str]) -> tuple[int, str]:
            return item
        return cls()

    async def run(self, count: int = 1) -> str:
        async def nested(label: str) -> None:
            pass
        return str(count)

    class Inner:
        @staticmethod
        def convert(value: bytes) -> str:
            return value.decode()

@decorator
def top(value: int) -> int:
    def nested_top(flag: bool = False) -> None:
        pass
    return value
`

func TestCodeMapPythonOracleSymbols(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "code_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name            string
			Sources         map[string]string              `json:"sources"`
			ExpectedSymbols map[string][][]json.RawMessage `json:"expected_symbols"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var source string
	var want []codeMapSymbol
	for _, c := range fixture.Cases {
		if c.Name != "p43_oracle_symbols" {
			continue
		}
		source = c.Sources["nested.py"]
		for _, row := range c.ExpectedSymbols["nested.py"] {
			var symbol codeMapSymbol
			if len(row) != 4 || json.Unmarshal(row[0], &symbol.Line) != nil || json.Unmarshal(row[1], &symbol.Kind) != nil || json.Unmarshal(row[2], &symbol.Name) != nil || json.Unmarshal(row[3], &symbol.Signature) != nil {
				t.Fatalf("invalid frozen oracle symbol: %v", row)
			}
			want = append(want, symbol)
		}
		break
	}
	if source == "" || source != pythonOracleSource || want == nil {
		t.Fatal("frozen Python oracle case missing or changed")
	}
	got, err := extractPythonSymbols([]byte(source))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols mismatch: err=%v\n got: %#v\nwant: %#v", err, got, want)
	}
}

func BenchmarkCodeMapPythonParse(b *testing.B) {
	synthetic := bytes.Repeat([]byte("value = 1\n"), 30000)
	for _, sample := range []struct {
		name string
		src  []byte
	}{{"fixture", []byte(pythonOracleSource)}, {"synthetic_300KB", synthetic}} {
		b.Run(sample.name, func(b *testing.B) {
			b.SetBytes(int64(len(sample.src)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := extractPythonSymbols(sample.src); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
