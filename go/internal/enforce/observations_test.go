package enforce

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"purser-hook/internal/testutil"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestObservationEventTypesMatchPythonSchema(t *testing.T) {
	// Copied from .claude/hooks/orchestrator/telemetry_schema.py: EVENT_TYPES.
	want := []string{
		"TASK_ROUTE_SELECTED", "IMPLEMENTER_STARTED", "IMPLEMENTER_FINISHED", "IMPLEMENTER_FAILED",
		"MECHANICAL_MODEL_RANK_CHANGED", "REVIEWER1_STARTED", "REVIEWER1_FINISHED",
		"COMPACTION_ADVICE_OUTCOME", "STOP_BLOCKED", "SHADOW_FINISHED", "SCOPE_CONFLICT_OUTCOME",
		"REVIEWER1_SKIPPED", "REVIEWER1_FAILED", "ROUTING_PROTOCOL_VIOLATION",
		"THREAD_CONTINUITY_VIOLATION", "UNKNOWN_CONCURRENT_WRITER", "WORKFLOW_VERDICT_RECORDED",
		"TELEMETRY_WARNING", "CLAUDE_TASK_USAGE_RECORDED", "CONTEXT_IO_TURN_RECORDED",
		"CODEX_RESULT_UNFETCHED", "CLAUDE_DECISION_RECORDED", "WORKER_REPLAY_CONFLICT",
		"MECHANICAL_EDIT_OBSERVED", "MAINTENANCE_REPORT_FINISHED", "MAINTENANCE_REPORT_ACTED",
		"READINESS_CHECK_OUTCOME",
		"PLAN_REDIRECT",
	}
	if len(observationEventTypes) != len(want) {
		t.Fatalf("Go event type count = %d, Python EVENT_TYPES count = %d", len(observationEventTypes), len(want))
	}
	for _, eventType := range want {
		if !observationEventTypes[eventType] {
			t.Errorf("Go event type set is missing Python EVENT_TYPES member %q", eventType)
		}
	}
	event := map[string]any{
		"schema_version": float64(1), "event_id": "123e4567-e89b-12d3-a456-426614174000",
		"ts_utc": "2026-10-04T12:00:00Z", "event_type": "WORKER_REPLAY_CONFLICT",
		"task_id": "task", "project_key": "0123456789abcdef", "session_id": "session",
		"repo": nil, "branch": nil, "agent_id": nil, "agent_type": nil, "payload": map[string]any{},
	}
	if !validObservationTaskEvent(event) {
		t.Fatal("valid WORKER_REPLAY_CONFLICT observation was rejected")
	}
}

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
	cmd := testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestObservationP49ProcessContention$"))
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
	cmd := testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestObservationP50Process$"))
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
		children[i] = testutil.Hide(exec.Command(os.Args[0], "-test.run=^TestObservationP50Process$"))
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

func TestObservationP51UsageParity(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Type, Malformed string
			Task                  any
			Envelope, Payload     map[string]any
			Snapshot, Expected    map[string]any
		} `json:"p51_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			var capturedAt string
			if c.Snapshot != nil {
				snapshot := make(map[string]any, len(c.Snapshot)+1)
				for key, value := range c.Snapshot {
					if key != "age_seconds" {
						snapshot[key] = value
					}
				}
				age, _ := c.Snapshot["age_seconds"].(float64)
				capturedAt = time.Now().UTC().Truncate(time.Second).Add(-time.Duration(age) * time.Second).Format(time.RFC3339Nano)
				snapshot["captured_at"] = capturedAt
				path := filepath.Join(root, "telemetry", "latest", "claude_usage.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			} else if c.Malformed != "" {
				path := filepath.Join(root, "telemetry", "latest", "claude_usage.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(c.Malformed), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := buildObservationAtStateRoot(c.Type, c.Envelope, c.Payload, c.Task, root)
			if err != nil {
				t.Fatal(err)
			}
			expected := make(map[string]any, len(c.Expected))
			for key, value := range c.Expected {
				if key == "claude_usage_captured_at" && value == "$CAPTURED_AT" {
					expected[key] = capturedAt
				} else {
					expected[key] = value
				}
			}
			if !reflect.DeepEqual(got["payload"], expected) {
				t.Fatalf("payload = %#v, want %#v", got["payload"], expected)
			}
		})
	}
}

func TestObservationP52AggregateParity(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name             string           `json:"name"`
			Task             any              `json:"task"`
			ProjectKey       any              `json:"project_key"`
			Existing         map[string]any   `json:"existing"`
			Events           []map[string]any `json:"events"`
			Expected         map[string]any   `json:"expected"`
			DecisionConflict bool             `json:"decision_conflict"`
		} `json:"p52_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, conflict := foldObservationAggregate(c.Existing, c.Task, c.ProjectKey, c.Events, true)
			gotJSON, err := canonicalJSON(got)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := canonicalJSON(c.Expected)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("aggregate mismatch\ngot:  %s\nwant: %s", gotJSON, wantJSON)
			}
			if conflict != c.DecisionConflict {
				t.Fatalf("decision conflict = %v, want %v", conflict, c.DecisionConflict)
			}
		})
	}
}

func TestObservationP53AggregateParity(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name        string           `json:"name"`
			Task        any              `json:"task"`
			ProjectKey  any              `json:"project_key"`
			Existing    map[string]any   `json:"existing"`
			Incremental bool             `json:"incremental"`
			Events      []map[string]any `json:"events"`
			Expected    map[string]any   `json:"expected"`
		} `json:"p53_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, _ := foldObservationAggregate(c.Existing, c.Task, c.ProjectKey, c.Events, c.Incremental)
			gotJSON, err := canonicalJSON(got)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := canonicalJSON(c.Expected)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("aggregate mismatch\ngot:  %s\nwant: %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestObservationP54UpdateTaskParity(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name                  string           `json:"name"`
			Task                  string           `json:"task"`
			ProjectKey            string           `json:"project_key"`
			IndexEvents           []map[string]any `json:"index_events"`
			Cursor                map[string]any   `json:"cursor"`
			SpoolEvents           []map[string]any `json:"spool_events"`
			MergedIDs             []string         `json:"merged_ids"`
			Existing              map[string]any   `json:"existing"`
			Verdict               map[string]any   `json:"verdict"`
			Recover               bool             `json:"recover_after_failed_replace"`
			Malformed             bool             `json:"malformed_cursor"`
			ExpectedTask          map[string]any   `json:"expected_task"`
			ExpectedCursor        map[string]any   `json:"expected_cursor"`
			ExpectedTaskExists    bool             `json:"expected_task_exists"`
			FailedCursorUnchanged bool             `json:"failed_replace_cursor_unchanged"`
		} `json:"p54_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			state := t.TempDir()
			root := filepath.Join(state, "telemetry")
			project := filepath.Join(state, "projects", c.ProjectKey, "telemetry")
			taskPath := filepath.Join(project, "tasks", c.Task+".json")
			cursorPath := filepath.Join(project, "tasks", ".cursors", c.Task+".json")
			if len(c.IndexEvents) > 0 {
				var index []byte
				for _, event := range c.IndexEvents {
					encoded, marshalErr := canonicalJSON(event)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					index = append(append(index, encoded...), '\n')
				}
				writeObservationFile(t, filepath.Join(root, "events", "by_task", c.Task+".jsonl"), index)
			}
			for _, event := range c.SpoolEvents {
				encoded, marshalErr := canonicalJSON(event)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				writeObservationFile(t, filepath.Join(root, "events", "spool", asString(event["event_id"])+".json"), encoded)
			}
			for _, id := range c.MergedIDs {
				writeObservationFile(t, filepath.Join(root, "events", "spool", ".merged", id), nil)
			}
			if c.Existing != nil {
				encoded, marshalErr := canonicalJSON(c.Existing)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				writeObservationFile(t, taskPath, encoded)
			}
			if c.Cursor != nil {
				encoded, marshalErr := canonicalJSON(c.Cursor)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				writeObservationFile(t, cursorPath, encoded)
			}
			if c.Verdict != nil {
				encoded, marshalErr := canonicalJSON(c.Verdict)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				writeObservationFile(t, filepath.Join(project, "verdicts", c.Task+".json"), encoded)
			}
			if c.Recover {
				if err = os.MkdirAll(filepath.Dir(taskPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(taskPath, 0700); err != nil {
					t.Fatal(err)
				}
				if updateErr := updateObservationTask(root, c.Task, c.ProjectKey); updateErr == nil {
					t.Fatal("task replacement unexpectedly succeeded")
				}
				if (os.IsNotExist(statErr(cursorPath))) != c.FailedCursorUnchanged {
					t.Fatal("failed replacement cursor state differs from Python")
				}
				if err = os.Remove(taskPath); err != nil {
					t.Fatal(err)
				}
			} else if c.Malformed {
				updateObservationTaskBestEffort(root, c.Task, c.ProjectKey)
				health := readObservation(t, filepath.Join(state, "telemetry", "telemetry_health.json"))
				if health["status"] != "DEGRADED" || health["last_error_code"] != "TELEMETRY_UPDATE_TASK_FAILED" {
					t.Fatalf("malformed cursor health marker = %#v", health)
				}
			} else if err = updateObservationTask(root, c.Task, c.ProjectKey); err != nil {
				t.Fatal(err)
			}
			if c.Recover {
				if err = updateObservationTask(root, c.Task, c.ProjectKey); err != nil {
					t.Fatal(err)
				}
			}
			if c.Malformed {
				if _, statErr := os.Stat(taskPath); (statErr == nil) != c.ExpectedTaskExists {
					t.Fatalf("task exists = %v, want %v", statErr == nil, c.ExpectedTaskExists)
				}
			} else {
				got := readObservation(t, taskPath)
				gotJSON, _ := canonicalJSON(got)
				wantJSON, _ := canonicalJSON(c.ExpectedTask)
				if !bytes.Equal(gotJSON, wantJSON) {
					t.Fatalf("task mismatch\ngot:  %s\nwant: %s", gotJSON, wantJSON)
				}
			}
			gotCursor := readObservation(t, cursorPath)
			gotJSON, _ := canonicalJSON(gotCursor)
			wantJSON, _ := canonicalJSON(c.ExpectedCursor)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("cursor mismatch: got %s want %s", gotJSON, wantJSON)
			}
		})
	}
}

func statErr(path string) error {
	_, err := os.Stat(path)
	return err
}

func writeObservationFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
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

func TestObservationP55ProjectionBoundaries(t *testing.T) {
	scratchBase, err := filepath.Abs(filepath.Join("..", "..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scratchBase, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observations.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name         string   `json:"name"`
			Config       string   `json:"config"`
			EnvRoot      *string  `json:"env_root"`
			ExplicitRoot *string  `json:"explicit_root"`
			Parts        []string `json:"parts"`
			Symlink      string   `json:"symlink"`
			Run          bool     `json:"run"`
			Content      string   `json:"content"`
			Edited       bool     `json:"edited"`
			Exists       bool     `json:"exists"`
			YAML         bool     `json:"yaml"`
			Expected     struct {
				Enabled      bool   `json:"enabled"`
				Root         string `json:"root"`
				Contained    bool   `json:"contained"`
				Marker       string `json:"marker"`
				Intact       bool   `json:"intact"`
				Replaceable  bool   `json:"replaceable"`
				RunContained bool   `json:"run_contained"`
			} `json:"expected"`
		} `json:"p55_cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			scratch, err := os.MkdirTemp(scratchBase, "p55-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(scratch)
			root, outside := filepath.Join(scratch, "vault"), filepath.Join(scratch, "outside")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(outside, 0700); err != nil {
				t.Fatal(err)
			}
			rootJSON, _ := json.Marshal(root)
			outsideJSON, _ := json.Marshal(outside)
			config := strings.ReplaceAll(tc.Config, "${ROOT}", string(rootJSON[1:len(rootJSON)-1]))
			config = strings.ReplaceAll(config, "${OUTSIDE}", string(outsideJSON[1:len(outsideJSON)-1]))
			var env, explicit *string
			if tc.EnvRoot != nil {
				v := strings.ReplaceAll(*tc.EnvRoot, "${ROOT}", root)
				env = &v
			}
			if tc.ExplicitRoot != nil {
				v := strings.ReplaceAll(*tc.ExplicitRoot, "${ROOT}", root)
				explicit = &v
			}
			if tc.Symlink != "" {
				link := filepath.Join(root, tc.Symlink)
				if err := os.Symlink(outside, link); err != nil {
					if junctionErr := testutil.Hide(exec.Command("cmd", "/c", "mklink", "/J", link, outside)).Run(); junctionErr != nil {
						t.Fatalf("symlink/junction unavailable: %v; %v", err, junctionErr)
					}
				}
			}
			enabled, vault := observationProjectionSettings([]byte(config), env, explicit)
			if enabled != tc.Expected.Enabled {
				t.Fatalf("enabled=%v want %v", enabled, tc.Expected.Enabled)
			}
			if tc.Expected.Root == "${ROOT}" && vault != root {
				t.Fatalf("vault=%q want %q", vault, root)
			}
			if tc.Expected.Root == "" && vault != "" {
				t.Fatalf("vault=%q want empty", vault)
			}
			parts := append([]string(nil), tc.Parts...)
			for i := range parts {
				if parts[i] == "@ABS" {
					parts[i] = filepath.Join(outside, "note.md")
				}
			}
			candidate, contained := observationSafeVaultPath(root, parts...)
			if contained != tc.Expected.Contained {
				t.Fatalf("contained=%v want %v (%s)", contained, tc.Expected.Contained, candidate)
			}
			if tc.Run {
				_, runContained := observationSafeRunPath(root, "0000", "00", "task")
				if runContained != tc.Expected.RunContained {
					t.Fatalf("run containment=%v want %v", runContained, tc.Expected.RunContained)
				}
			}
			marked := observationWithMarker("", tc.Content, tc.YAML)
			if observationMarkerFor(tc.Content, tc.YAML) != tc.Expected.Marker {
				t.Fatalf("marker mismatch: %q", observationMarkerFor(tc.Content, tc.YAML))
			}
			if tc.Edited {
				marked += "edited"
			}
			intact := observationGeneratedContentIsIntact(marked)
			if intact != tc.Expected.Intact {
				t.Fatalf("intact=%v want %v", intact, tc.Expected.Intact)
			}
			if observationMayReplaceExisting(tc.Exists, marked) != tc.Expected.Replaceable {
				t.Fatal("generated replacement decision mismatch")
			}
			if tc.Exists && observationMayReplaceExisting(true, observationWithMarker("", tc.Content, tc.YAML)+"edited") {
				t.Fatal("edited generated content was replaceable")
			}
		})
	}
}

func TestObservationP56RunNoteRendering(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observation_projection.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name            string         `json:"name"`
			Aggregate       map[string]any `json:"aggregate"`
			ExpectedContent string         `json:"expected_content"`
			ExpectedPath    string         `json:"expected_path"`
			Invalid         bool           `json:"invalid"`
			Error           string         `json:"error"`
		} `json:"p56_cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	base, err := filepath.Abs(filepath.Join("..", "..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			vault := filepath.Join(base, tc.Name, "vault")
			if err := os.MkdirAll(vault, 0700); err != nil {
				t.Fatal(err)
			}
			content, path, renderErr := renderObservationRunNote(tc.Aggregate, vault)
			if tc.Invalid {
				if renderErr == nil {
					t.Fatal("invalid aggregate rendered")
				}
				if !strings.Contains(renderErr.Error(), tc.Error) {
					t.Fatalf("error %q does not contain %q", renderErr, tc.Error)
				}
				return
			}
			if renderErr != nil {
				t.Fatal(renderErr)
			}
			if content != tc.ExpectedContent {
				gotLines, wantLines := strings.Split(content, "\n"), strings.Split(tc.ExpectedContent, "\n")
				for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
					if gotLines[i] != wantLines[i] {
						t.Fatalf("rendered note differs at line %d: got %q, want %q", i+1, gotLines[i], wantLines[i])
					}
				}
				t.Fatalf("rendered note line count differs: got %d, want %d", len(gotLines), len(wantLines))
			}
			want := strings.ReplaceAll(tc.ExpectedPath, "${VAULT}", vault)
			if filepath.Clean(path) != filepath.Clean(want) {
				t.Fatalf("path = %q, want %q", path, want)
			}
		})
	}
}

func TestObservationP57HealthNoteRendering(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observation_projection.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Timezone string `json:"timezone"`
		Cases    []struct {
			Name          string            `json:"name"`
			TaskFiles     map[string]string `json:"task_files"`
			Unreadable    string            `json:"unreadable_task"`
			Expected      string            `json:"expected_content"`
			ExpectedPath  string            `json:"expected_path"`
			ExpectedError string            `json:"expected_error"`
		} `json:"p57_cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Timezone != "Europe/Istanbul" || observationDisplayTimezone != "Europe/Istanbul" {
		t.Fatalf("timezone fixture=%q Go=%q", fixture.Timezone, observationDisplayTimezone)
	}
	base, err := filepath.Abs(filepath.Join("..", "..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			caseRoot, err := os.MkdirTemp(base, "p57-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(caseRoot)
			state := filepath.Join(caseRoot, "state")
			tasks := filepath.Join(state, "telemetry", "tasks")
			if err := os.MkdirAll(tasks, 0700); err != nil {
				t.Fatal(err)
			}
			for name, raw := range tc.TaskFiles {
				if err := os.WriteFile(filepath.Join(tasks, name), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.Unreadable != "" {
				if err := os.Mkdir(filepath.Join(tasks, tc.Unreadable), 0700); err != nil {
					t.Fatal(err)
				}
			}
			vault := filepath.Join(caseRoot, "vault")
			content, path, renderErr := renderObservationHealthNote(state, vault, nil)
			if tc.ExpectedError != "" {
				if renderErr == nil || !strings.Contains(renderErr.Error(), tc.ExpectedError) {
					t.Fatalf("error = %v, want containing %q", renderErr, tc.ExpectedError)
				}
				return
			}
			if renderErr != nil {
				t.Fatal(renderErr)
			}
			if content != tc.Expected {
				t.Fatalf("health note differs:\n got %q\nwant %q", content, tc.Expected)
			}
			want := strings.ReplaceAll(tc.ExpectedPath, "${VAULT}", vault)
			if filepath.Clean(path) != filepath.Clean(want) {
				t.Fatalf("path = %q, want %q", path, want)
			}
		})
	}
}

func TestObservationP58ProjectionWrites(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/projection_writes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Clock     string         `json:"clock"`
		Aggregate map[string]any `json:"aggregate"`
		Cases     []struct {
			Name                string         `json:"name"`
			Enabled             *bool          `json:"enabled"`
			InitialHealth       map[string]any `json:"initial_health"`
			RunNoteDirectory    bool           `json:"run_note_directory"`
			HealthDirectory     bool           `json:"health_directory"`
			HealthNoteDirectory bool           `json:"health_note_directory"`
		} `json:"cases"`
		Expected []struct {
			Name       string         `json:"name"`
			RunNote    *string        `json:"run_note"`
			HealthJSON map[string]any `json:"health_json"`
			HealthNote *string        `json:"health_note"`
		} `json:"expected"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != len(fixture.Expected) {
		t.Fatalf("fixture cases=%d expected=%d", len(fixture.Cases), len(fixture.Expected))
	}
	parsedClock, err := time.Parse(time.RFC3339Nano, fixture.Clock)
	if err != nil {
		t.Fatal(err)
	}
	oldClock := observationProjectionNow
	observationProjectionNow = func() time.Time { return parsedClock }
	defer func() { observationProjectionNow = oldClock }()
	base, err := filepath.Abs(filepath.Join("..", "..", "..", ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	for index, tc := range fixture.Cases {
		want := fixture.Expected[index]
		if tc.Name != want.Name || !strings.HasPrefix(tc.Name, "p58-") {
			t.Fatalf("case/expected names differ: %q %q", tc.Name, want.Name)
		}
		t.Run(tc.Name, func(t *testing.T) {
			caseRoot := filepath.Join(base, tc.Name)
			if err := os.RemoveAll(caseRoot); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(caseRoot, "state")
			root := filepath.Join(state, "telemetry")
			vault := filepath.Join(caseRoot, "vault")
			projectTasks := filepath.Join(state, "projects", "p58-project", "telemetry", "tasks")
			if err := os.MkdirAll(projectTasks, 0700); err != nil {
				t.Fatal(err)
			}
			aggregate, err := json.Marshal(fixture.Aggregate)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(projectTasks, "p58-task.json"), aggregate, 0600); err != nil {
				t.Fatal(err)
			}
			healthPath := filepath.Join(root, "telemetry_health.json")
			if tc.HealthDirectory {
				if err = os.MkdirAll(healthPath, 0700); err != nil {
					t.Fatal(err)
				}
			} else if tc.InitialHealth != nil {
				health, _ := json.Marshal(tc.InitialHealth)
				if err = os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(healthPath, health, 0600); err != nil {
					t.Fatal(err)
				}
			}
			started, _ := fixture.Aggregate["started_at"].(string)
			runDate, _ := time.Parse(time.RFC3339Nano, started)
			runPath := filepath.Join(vault, "Runs", runDate.In(observationDisplayZone).Format("2006"), runDate.In(observationDisplayZone).Format("01"), "p58-task.md")
			if tc.RunNoteDirectory {
				if err = os.MkdirAll(runPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			healthNotePath := filepath.Join(vault, "System", "Telemetry Health.md")
			if tc.HealthNoteDirectory {
				if err = os.MkdirAll(healthNotePath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			enabled := true
			if tc.Enabled != nil {
				enabled = *tc.Enabled
			}
			config := map[string]any{"obsidian_projection": map[string]any{"enabled": enabled, "vault_root": vault}}
			configData, _ := json.Marshal(config)
			configPath := filepath.Join(caseRoot, "config.json")
			if err = os.MkdirAll(caseRoot, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(configPath, configData, 0600); err != nil {
				t.Fatal(err)
			}
			projectObservationTask(root, "p58-task", "p58-project", configPath, nil)
			if got := readObservationFixtureFile(t, runPath); want.RunNote != nil && got != *want.RunNote {
				t.Fatalf("run note differs\n got: %q\nwant: %q", got, *want.RunNote)
			} else if want.RunNote == nil && got != "" && !tc.RunNoteDirectory {
				t.Fatalf("unexpected run note: %q", got)
			}
			if got, err := os.ReadFile(healthPath); err == nil {
				var gotHealth map[string]any
				gotDecoder := json.NewDecoder(bytes.NewReader(got))
				gotDecoder.UseNumber()
				if gotDecoder.Decode(&gotHealth) != nil || !reflect.DeepEqual(gotHealth, want.HealthJSON) {
					t.Fatalf("health JSON = %#v, want %#v", gotHealth, want.HealthJSON)
				}
			} else if want.HealthJSON != nil {
				t.Fatalf("health JSON read: %v", err)
			}
			if got := readObservationFixtureFile(t, healthNotePath); want.HealthNote != nil && got != *want.HealthNote {
				t.Fatalf("health note differs\n got: %q\nwant: %q", got, *want.HealthNote)
			} else if want.HealthNote == nil && got != "" && !tc.HealthNoteDirectory {
				t.Fatalf("unexpected health note: %q", got)
			}
			err = filepath.WalkDir(caseRoot, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tmp") {
					return fmt.Errorf("temporary file left after projection: %s", path)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func readObservationFixtureFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestObservationP56AcceptsControlledUsageEnums(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/observation_projection.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Aggregate map[string]any `json:"aggregate"`
			Invalid   bool           `json:"invalid"`
		} `json:"p56_cases"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		usage, ok := tc.Aggregate["usage"].(map[string]any)
		if tc.Invalid || !ok {
			continue
		}
		usage["claude_rate_limit_delta_scope"] = "account_window_observation"
		usage["claude_task_usage_source"] = "desktop_jsonl_observed"
		usage["claude_task_usage_boundary"] = "anchored_pre_tool_to_stop"
		usage["claude_task_usage_dedup"] = "message_id_request_id"
		run, err := prepareObservationRun(tc.Aggregate)
		if err != nil {
			t.Fatal(err)
		}
		if got := run.values["usage"].(map[string]any)["claude_task_usage_source"]; got != "desktop_jsonl_observed" {
			t.Fatalf("enum value not preserved: %v", got)
		}
		usage["claude_task_usage_dedup"] = "other"
		if _, err = prepareObservationRun(tc.Aggregate); err == nil || !strings.Contains(err.Error(), "controlled enum") {
			t.Fatalf("out-of-enum value accepted: %v", err)
		}
		return
	}
	t.Fatal("no valid fixture aggregate with usage")
}
