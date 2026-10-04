package enforce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/contextio"
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

func TestObservationP49Golden(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Mode, Type  string
			Task              any
			Envelope, Payload map[string]any
		} `json:"p49_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			event, err := buildObservation(c.Type, c.Envelope, c.Payload, c.Task)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if c.Mode == "spool" {
				release, lockErr := contextio.LockFile(filepath.Join(root, "events", ".append.lock"))
				if lockErr != nil {
					t.Fatal(lockErr)
				}
				code, persistErr := persistObservation(root, event)
				if unlockErr := release(); unlockErr != nil {
					t.Fatal(unlockErr)
				}
				if code != "" || persistErr != nil {
					t.Fatalf("spool = %q, %v", code, persistErr)
				}
				stored := readObservation(t, filepath.Join(root, "events", "spool", event["event_id"].(string)+".json"))
				if !sameObservation(t, stored, event) {
					t.Fatal("spool changed event identity or evidence")
				}
				if _, err = os.Stat(filepath.Join(root, "events", "by_task", c.Task.(string)+".jsonl")); !os.IsNotExist(err) {
					t.Fatalf("spooled event claimed as indexed: %v", err)
				}
				return
			}
			code, persistErr := persistObservation(root, event)
			if code != "" || persistErr != nil {
				t.Fatalf("append = %q, %v", code, persistErr)
			}
			stored := readObservationLine(t, filepath.Join(root, "events", event["ts_utc"].(string)[:7], "session_p49-session.jsonl"))
			indexed := readObservationLine(t, filepath.Join(root, "events", "by_task", c.Task.(string)+".jsonl"))
			if !sameObservation(t, stored, event) || !sameObservation(t, indexed, event) {
				t.Fatal("append/index changed event identity or evidence")
			}
		})
	}
}

func TestObservationP49ProcessContention(t *testing.T) {
	if root := os.Getenv("P49_OBSERVATION_CHILD_ROOT"); root != "" {
		event := map[string]any{"event_id": "p49-process-id", "ts_utc": "2026-10-04T12:00:00Z", "session_id": "process-session", "task_id": "process-task", "payload": map[string]any{"evidence": "kept"}}
		if _, err := persistObservation(root, event); err != nil {
			t.Fatal(err)
		}
		return
	}
	root := t.TempDir()
	release, err := contextio.LockFile(filepath.Join(root, "events", ".append.lock"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestObservationP49ProcessContention$")
	cmd.Env = append(os.Environ(), "P49_OBSERVATION_CHILD_ROOT="+root)
	output, err := cmd.CombinedOutput()
	if unlockErr := release(); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	spooled := readObservation(t, filepath.Join(root, "events", "spool", "p49-process-id.json"))
	if spooled["event_id"] != "p49-process-id" || spooled["task_id"] != "process-task" || spooled["payload"].(map[string]any)["evidence"] != "kept" {
		t.Fatalf("spool lost identity/evidence: %#v", spooled)
	}
	if _, err = os.Stat(filepath.Join(root, "events", "by_task", "process-task.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("spooled event claimed as indexed: %v", err)
	}
}

func TestObservationP49FailureBranches(t *testing.T) {
	event := map[string]any{"event_id": "p49-failure-id", "ts_utc": "2026-10-04T12:00:00Z", "session_id": "session", "task_id": "task", "payload": map[string]any{"evidence": "kept"}}
	tests := []struct {
		name     string
		setup    func(string) func()
		mutate   func(map[string]any)
		wantCode string
	}{
		{"invalid-id", func(string) func() { return func() {} }, func(e map[string]any) { e["event_id"] = "../bad" }, "TELEMETRY_EMIT_FAILED"},
		{"lock-open", func(root string) func() {
			_ = os.MkdirAll(filepath.Join(root, "events"), 0700)
			_ = os.Mkdir(filepath.Join(root, "events", ".append.lock"), 0700)
			return func() {}
		}, func(map[string]any) {}, "TELEMETRY_EMIT_FAILED"},
		{"append", func(root string) func() {
			_ = os.MkdirAll(filepath.Join(root, "events", "2026-10"), 0700)
			_ = os.Mkdir(filepath.Join(root, "events", "2026-10", "session_session.jsonl"), 0700)
			return func() {}
		}, func(map[string]any) {}, "TELEMETRY_EMIT_FAILED"},
		{"index", func(root string) func() {
			_ = os.MkdirAll(filepath.Join(root, "events", "by_task"), 0700)
			_ = os.Mkdir(filepath.Join(root, "events", "by_task", "task.jsonl"), 0700)
			return func() {}
		}, func(map[string]any) {}, "TELEMETRY_EMIT_FAILED"},
		{"spool", func(root string) func() {
			release, _ := contextio.LockFile(filepath.Join(root, "events", ".append.lock"))
			_ = os.MkdirAll(filepath.Join(root, "events", "spool"), 0700)
			_ = os.Mkdir(filepath.Join(root, "events", "spool", "p49-failure-id.json"), 0700)
			return func() { _ = release() }
		}, func(map[string]any) {}, "TELEMETRY_EMIT_FAILED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			release := tt.setup(root)
			defer release()
			copy := map[string]any{}
			for k, v := range event {
				copy[k] = v
			}
			tt.mutate(copy)
			code, err := persistObservation(root, copy)
			if err == nil || code != tt.wantCode {
				t.Fatalf("result=%q,%v; want failure %q", code, err, tt.wantCode)
			}
		})
	}
}

func readObservation(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func readObservationLine(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	var value map[string]any
	if err = json.Unmarshal([]byte(line), &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func sameObservation(t *testing.T, left, right map[string]any) bool {
	t.Helper()
	a, err := canonicalJSON(left)
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonicalJSON(right)
	if err != nil {
		t.Fatal(err)
	}
	return string(a) == string(b)
}
