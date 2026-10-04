package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPythonRouteThreadGoldenParity(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "route_thread.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name         string         `json:"name"`
		Kind         string         `json:"kind"`
		Input        map[string]any `json:"input"`
		ExpectedCode string         `json:"expected_code"`
		Result       map[string]any `json:"result"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var got any
			switch tc.Kind {
			case "route":
				got = ResolveRoute(tc.Input["route"], tc.Input["config"].(map[string]any))
			case "compatibility":
				got = ThreadCompatibility(tc.Input["requested"], tc.Input["recorded"])
			case "fresh":
				got = DecideFreshCodex(tc.Input["requested"], tc.Input["recorded"], tc.Input["reason"])
			default:
				t.Fatalf("unknown fixture kind %q", tc.Kind)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(gotJSON, &fields); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fields, tc.Result) {
				t.Fatalf("result mismatch\n got: %#v\nwant: %#v", fields, tc.Result)
			}
			if tc.ExpectedCode != "" && fields["code"] != tc.ExpectedCode {
				t.Fatalf("rejection hit code %v, want guarded branch %s", fields["code"], tc.ExpectedCode)
			}
			if tc.Kind == "route" && tc.ExpectedCode != "" && (fields["model"] != nil || fields["effort"] != nil) {
				t.Fatalf("invalid route selected fallback pair: %#v", fields)
			}
			if tc.Name == "valid-route" && fields["valid"] != true {
				t.Fatalf("valid route was rejected: %#v", fields)
			}
			if tc.Name == "valid-thread" && fields["compatible"] != true {
				t.Fatalf("valid thread was rejected: %#v", fields)
			}
		})
	}
}
