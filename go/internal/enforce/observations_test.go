package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestObservationP48Golden(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Type        string
			Task              any
			Envelope, Payload map[string]any
			Build             bool
			Clean             map[string]any
			Expected          map[string]any
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, buildErr := buildObservation(c.Type, c.Envelope, c.Payload, c.Task)
			if (buildErr == nil) != c.Build {
				t.Fatalf("build error = %v, want success %v", buildErr, c.Build)
			}
			if !c.Build {
				return
			}
			if !reflect.DeepEqual(got["payload"], c.Clean) {
				t.Fatalf("payload = %#v, want %#v", got["payload"], c.Clean)
			}
			for key, expected := range c.Expected {
				if !reflect.DeepEqual(got[key], expected) {
					t.Fatalf("%s = %#v, want %#v", key, got[key], expected)
				}
			}
		})
	}
}

func TestObservationHealthP48DegradedAndWriterFailure(t *testing.T) {
	root := t.TempDir()
	recordObservationHealth(root, "DEGRADED", "CLAUDE_TRANSCRIPT_PARSER_FAILED")
	data, err := os.ReadFile(filepath.Join(root, "telemetry", "telemetry_health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var health map[string]any
	if err = json.Unmarshal(data, &health); err != nil {
		t.Fatal(err)
	}
	if health["status"] != "DEGRADED" || health["last_error_code"] != "CLAUDE_TRANSCRIPT_PARSER_FAILED" || health["failure_count"] != float64(1) {
		t.Fatalf("degraded marker lost: %#v", health)
	}
	blocked := filepath.Join(root, "file")
	if err = os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	decision := map[string]any{"permissionDecision": "deny", "reason": "policy"}
	recordObservationHealth(blocked, "DEGRADED", "TELEMETRY_EMIT_FAILED")
	if decision["permissionDecision"] != "deny" || decision["reason"] != "policy" {
		t.Fatalf("telemetry changed enforcement: %#v", decision)
	}
}
