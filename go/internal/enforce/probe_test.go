package enforce

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestProbeP38PythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/probe.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Commands []struct {
			Prefix   []string `json:"prefix"`
			Cwd      string   `json:"cwd"`
			Model    string   `json:"model"`
			Output   string   `json:"output"`
			Expected []string `json:"expected"`
		} `json:"commands"`
		Cases []struct {
			Name       string         `json:"name"`
			Stdout     string         `json:"stdout"`
			Stderr     string         `json:"stderr"`
			OutputData bool           `json:"output_data"`
			Started    bool           `json:"started"`
			Returncode int            `json:"returncode"`
			TimedOut   bool           `json:"timed_out"`
			Expected   map[string]any `json:"expected"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, item := range fixture.Commands {
		if got := ProbeCommand(item.Prefix, item.Cwd, item.Model, item.Output); !reflect.DeepEqual(got, item.Expected) {
			t.Errorf("command p38 = %#v, want %#v", got, item.Expected)
		}
	}
	for _, item := range fixture.Cases {
		got := ProbeResult([]byte(item.Stdout), item.Stderr, item.OutputData, item.Started, item.Returncode, item.TimedOut)
		if !reflect.DeepEqual(got, item.Expected) {
			t.Errorf("%s: got %#v, want %#v", item.Name, got, item.Expected)
		}
		if got["ok"] == true && item.Name != "p38_completed" {
			t.Errorf("%s reported success", item.Name)
		}
	}
}
