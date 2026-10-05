package ledger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestDeltaPythonFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_baseline", "delta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name     string            `json:"name"`
			Kind     string            `json:"kind"`
			Raw      string            `json:"raw"`
			Baseline []map[string]any  `json:"baseline"`
			Current  []map[string]any  `json:"current"`
			Result   map[string]any    `json:"result"`
			Error    map[string]string `json:"error"`
		} `json:"cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			var result map[string]any
			var err error
			switch test.Kind {
			case "names":
				paths := parseNameList([]byte(test.Raw))
				ordered := make([]string, 0, len(paths))
				for path := range paths {
					ordered = append(ordered, path)
				}
				sort.Strings(ordered)
				result = map[string]any{"paths": ordered}
			case "numstat":
				var counts map[string]int
				counts, err = parseNumstat([]byte(test.Raw))
				result = map[string]any{"counts": counts}
			case "manifest":
				var paths map[string]bool
				var counts map[string]int
				paths, counts, err = manifestDelta(test.Baseline, test.Current)
				ordered := make([]string, 0, len(paths))
				for path := range paths {
					ordered = append(ordered, path)
				}
				sort.Strings(ordered)
				result = map[string]any{"paths": ordered, "counts": counts}
			default:
				t.Fatalf("unknown case kind %q", test.Kind)
			}
			if test.Error != nil {
				failure, ok := err.(*inspectionError)
				if !ok || failure.Code != test.Error["code"] || failure.Message != test.Error["message"] {
					t.Fatalf("error = %v, want %#v", err, test.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result)
			var got any
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			encoded, _ = json.Marshal(test.Result)
			var want any
			decoder = json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			if err := decoder.Decode(&want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("result = %#v, want %#v", result, test.Result)
			}
		})
	}
}
