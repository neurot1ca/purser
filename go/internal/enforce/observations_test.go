package enforce

import (
	"bytes"
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

func TestObservationP50Golden(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name       string         `json:"name"`
			Mode       string         `json:"mode"`
			PythonLine string         `json:"python_line"`
			Filename   string         `json:"filename"`
			Raw        string         `json:"raw"`
			HealthCode string         `json:"health_code"`
			Event      map[string]any `json:"event"`
		} `json:"p50_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			spool := filepath.Join(root, "events", "spool")
			if err := os.MkdirAll(spool, 0700); err != nil {
				t.Fatal(err)
			}
			if c.Mode == "merge" {
				if !validObservationSpoolEvent(c.Event) {
					t.Fatal("fixture event rejected")
				}
				if err := os.WriteFile(filepath.Join(spool, c.Event["event_id"].(string)+".json"), []byte(strings.TrimSuffix(c.PythonLine, "\n")), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				raw := []byte(c.Raw)
				if c.Event != nil {
					raw, err = canonicalJSON(c.Event)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err = os.WriteFile(filepath.Join(spool, c.Filename), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			trigger := observationP50Trigger(t)
			code, persistErr := persistObservation(root, trigger)
			if code != "" || persistErr != nil {
				t.Fatalf("merge trigger = %q, %v", code, persistErr)
			}
			if c.Mode == "reject" {
				if _, statErr := os.Stat(filepath.Join(spool, c.Filename)); !os.IsNotExist(statErr) {
					t.Fatalf("rejected spool retained: %v", statErr)
				}
				health := readObservation(t, filepath.Join(root, "telemetry", "telemetry_health.json"))
				if health["last_error_code"] != c.HealthCode {
					t.Fatalf("rejection health = %#v", health)
				}
				return
			}
			eventID := c.Event["event_id"].(string)
			eventPath := filepath.Join(root, "events", "2026-10", "session_"+c.Event["session_id"].(string)+".jsonl")
			indexPath := filepath.Join(root, "events", "by_task", c.Event["task_id"].(string)+".jsonl")
			for _, path := range []string{eventPath, indexPath} {
				raw, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !bytes.Contains(raw, []byte(c.PythonLine)) {
					t.Fatalf("Python record bytes missing from %s: %q", path, raw)
				}
			}
			if _, statErr := os.Stat(filepath.Join(spool, ".merged", eventID)); statErr != nil {
				t.Fatalf("merge marker missing: %v", statErr)
			}
		})
	}
}

func TestObservationP50CrashRecoveryAndConcurrentMerge(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Mode       string         `json:"mode"`
			PythonLine string         `json:"python_line"`
			Event      map[string]any `json:"event"`
		} `json:"p50_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var seed map[string]any
	for _, c := range fixture.Cases {
		if c.Mode == "merge" {
			seed = c.Event
			break
		}
	}
	if seed == nil {
		t.Fatal("missing p50 merge fixture")
	}
	root := t.TempDir()
	spool := filepath.Join(root, "events", "spool")
	if err = os.MkdirAll(spool, 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if c.Mode == "merge" {
			id := c.Event["event_id"].(string)
			if err = os.WriteFile(filepath.Join(spool, id+".json"), []byte(strings.TrimSuffix(c.PythonLine, "\n")), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A child exits after the canonical append, leaving the Python marker and
	// spool file in place. The next real merge must finish only the task index.
	cmd := exec.Command(os.Args[0], "-test.run=^TestObservationP50Process$")
	cmd.Env = append(os.Environ(), "P50_OBSERVATION_MODE=crash", "P50_OBSERVATION_ROOT="+root)
	if output, runErr := cmd.CombinedOutput(); runErr == nil || !strings.Contains(runErr.Error(), "exit status 77") {
		t.Fatalf("crash child = %v, output %s", runErr, output)
	}
	trigger := observationP50Trigger(t)
	if code, mergeErr := persistObservation(root, trigger); code != "" || mergeErr != nil {
		t.Fatalf("recovery = %q, %v", code, mergeErr)
	}
	for _, c := range fixture.Cases {
		if c.Mode != "merge" {
			continue
		}
		id := c.Event["event_id"].(string)
		canonical := filepath.Join(root, "events", "2026-10", "session_"+c.Event["session_id"].(string)+".jsonl")
		index := filepath.Join(root, "events", "by_task", c.Event["task_id"].(string)+".jsonl")
		if countObservationID(t, canonical, id) != 1 || countObservationID(t, index, id) != 1 {
			t.Fatalf("recovery lost or duplicated record %s", id)
		}
		if _, statErr := os.Stat(filepath.Join(spool, id+".json")); !os.IsNotExist(statErr) {
			t.Fatalf("recovered spool retained: %v", statErr)
		}
	}

	concurrentRoot := t.TempDir()
	concurrentSpool := filepath.Join(concurrentRoot, "events", "spool")
	if err = os.MkdirAll(concurrentSpool, 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		if c.Mode != "merge" {
			continue
		}
		id := c.Event["event_id"].(string)
		if err = os.WriteFile(filepath.Join(concurrentSpool, id+".json"), []byte(strings.TrimSuffix(c.PythonLine, "\n")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	children := make([]*exec.Cmd, 2)
	var outputs [2]bytes.Buffer
	for i := range children {
		children[i] = exec.Command(os.Args[0], "-test.run=^TestObservationP50Process$")
		children[i].Env = append(os.Environ(), "P50_OBSERVATION_MODE=merge", "P50_OBSERVATION_ROOT="+concurrentRoot)
		children[i].Stdout, children[i].Stderr = &outputs[i], &outputs[i]
		if err = children[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, child := range children {
		if waitErr := child.Wait(); waitErr != nil {
			t.Fatalf("merge child: %v %s", waitErr, outputs[i].String())
		}
	}
	for _, c := range fixture.Cases {
		if c.Mode != "merge" {
			continue
		}
		id := c.Event["event_id"].(string)
		if countObservationID(t, filepath.Join(concurrentRoot, "events", "2026-10", "session_"+c.Event["session_id"].(string)+".jsonl"), id) != 1 {
			t.Fatalf("concurrent canonical count for %s is not one", id)
		}
	}
}

func TestObservationP50Process(t *testing.T) {
	mode, root := os.Getenv("P50_OBSERVATION_MODE"), os.Getenv("P50_OBSERVATION_ROOT")
	if mode == "" || root == "" {
		return
	}
	if mode == "crash" {
		spool := filepath.Join(root, "events", "spool")
		entries, err := os.ReadDir(spool)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Join(spool, ".merged"), 0700); err != nil {
			t.Fatal(err)
		}
		processed := 0
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(spool, entry.Name())
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			event := readObservation(t, path)
			id := event["event_id"].(string)
			if err = os.WriteFile(filepath.Join(spool, ".merged", id), nil, 0600); err != nil {
				t.Fatal(err)
			}
			line := append(append([]byte(nil), raw...), '\n')
			canonical := filepath.Join(root, "events", "2026-10", "session_"+event["session_id"].(string)+".jsonl")
			if err = appendObservationRecord(canonical, line[:len(line)-1]); err != nil {
				t.Fatal(err)
			}
			if processed > 0 {
				index := filepath.Join(root, "events", "by_task", event["task_id"].(string)+".jsonl")
				if err = appendObservationRecord(index, line[:len(line)-1]); err != nil {
					t.Fatal(err)
				}
			}
			processed++
		}
		if processed != 2 {
			t.Fatalf("crash staged %d records, want 2", processed)
		}
		os.Exit(77)
	}
	if mode != "merge" {
		t.Fatalf("unknown mode %q", mode)
	}
	if code, err := persistObservation(root, observationP50Trigger(t)); code != "" || err != nil {
		t.Fatalf("process merge = %q, %v", code, err)
	}
}

func observationP50Trigger(t *testing.T) map[string]any {
	t.Helper()
	event, err := buildObservation("TASK_ROUTE_SELECTED", map[string]any{"session_id": "p50-trigger"}, map[string]any{"phase": "implementation", "route": "normal", "model": "openai/gpt-5", "model_source": "route", "effort": "high", "routing_enforced": true, "fresh_thread": false, "reply_call": true}, "p50-trigger")
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func countObservationID(t *testing.T, path, id string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["event_id"] == id {
			count++
		}
	}
	return count
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
