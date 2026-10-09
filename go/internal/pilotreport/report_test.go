package pilotreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, includeDispatcher, mismatched bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	projects := filepath.Join(root, "projects")
	write := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(state, "codex_exec_mcp", "jobs", "job-1.json"), `{"taskId":"task-1","startedAt":1791547200,"finishedAt":1791547210,"appliedAt":1791547220,"applyState":"applied","acceptance":{"sessionId":"session-1","revision":"rev","acceptedAt":"2026-10-09T12:00:15Z"},"model":"worker-x","effort":"high","route":"dispatcher","usage":{}}`)
	events := []string{
		`{"event_type":"IMPLEMENTER_STARTED","ts_utc":"2026-10-09T12:00:00Z","session_id":"session-1","agent_id":"dispatcher-1"}`,
		`{"event_type":"REVIEWER1_STARTED","ts_utc":"2026-10-09T12:00:02Z","session_id":"session-1"}`,
		`{"event_type":"REVIEWER1_FINISHED","ts_utc":"2026-10-09T12:00:05Z","session_id":"session-1"}`,
		`{"event_type":"CLAUDE_DECISION_RECORDED","ts_utc":"2026-10-09T12:00:15Z","session_id":"session-1"}`,
		`{"event_type":"IMPLEMENTER_FINISHED","ts_utc":"2026-10-09T12:00:10Z","session_id":"session-1"}`,
	}
	write(filepath.Join(state, "telemetry", "events", "by_task", "task-1.jsonl"), strings.Join(events, "\n")+"\n")
	main := `{"type":"assistant","timestamp":"2026-10-09T12:00:01Z","requestId":"req-1","message":{"id":"msg-1","role":"assistant","model":"claude-main","content":[{"type":"tool_use","id":"tool-1","name":"mcp__codex-main__codex","input":{"jobId":"job-1","taskId":"task-1"}}],"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":1}}}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-10-09T12:00:01Z","requestId":"req-1","message":{"id":"msg-1","role":"assistant","model":"claude-main","content":[{"type":"tool_use","id":"tool-1","name":"mcp__codex-main__codex","input":{"jobId":"job-1","taskId":"task-1"}}],"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":1}}}}` + "\n" +
		`{"type":"user","timestamp":"2026-10-09T12:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"completed job-1 task-1"}]}}` + "\n" +
		`{"type":"assistant","requestId":"unrelated","message":{"id":"unrelated","role":"assistant","model":"<synthetic>","content":[],"usage":{"input_tokens":999,"output_tokens":999}}}` + "\n"
	write(filepath.Join(projects, "project-a", "session-1.jsonl"), main)
	if includeDispatcher {
		name := "agent-dispatcher-1.jsonl"
		if mismatched {
			name = "agent-other.jsonl"
		}
		dispatch := `{"type":"assistant","requestId":"d1","message":{"id":"d1","role":"assistant","model":"claude-dispatch","content":[],"usage":{"input_tokens":7,"output_tokens":5,"cache_read_input_tokens":2}}}` + "\n" +
			`{"type":"assistant","requestId":"d1","message":{"id":"d1","role":"assistant","model":"claude-dispatch","content":[],"usage":{"input_tokens":7,"output_tokens":5,"cache_read_input_tokens":2}}}` + "\n"
		launch := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"launch-job-1","name":"mcp__codex__codex_start","input":{"jobId":"job-1","taskId":"task-1"}}]}}` + "\n"
		dispatch = launch + dispatch
		write(filepath.Join(projects, "project-a", "session-1", "subagents", name), dispatch)
	}
	return state, projects
}

func TestPilotReport(t *testing.T) {
	tests := []struct {
		name                 string
		dispatcher, mismatch bool
		check                func(*testing.T, Report)
	}{
		{name: "happy path deduplicates and separates arms", dispatcher: true, check: func(t *testing.T, r Report) {
			if r.Usage.Main == nil || *r.Usage.Main.Input != 10 || *r.Usage.Main.Output != 4 || *r.Usage.Main.CacheRead != 3 {
				t.Fatalf("main usage = %#v", r.Usage.Main)
			}
			if r.Usage.Main.CacheCreation == nil || *r.Usage.Main.CacheCreation.Ephemeral5m != 2 || *r.Usage.Main.CacheCreation.Ephemeral1h != 1 {
				t.Fatalf("main cache = %#v", r.Usage.Main.CacheCreation)
			}
			if r.Usage.Dispatcher == nil || *r.Usage.Dispatcher.Input != 7 || *r.Usage.Dispatcher.Output != 5 {
				t.Fatalf("dispatcher usage = %#v", r.Usage.Dispatcher)
			}
			if r.Usage.CoordinationTurns == nil || *r.Usage.CoordinationTurns != 1 {
				t.Fatalf("turns = %#v", r.Usage.CoordinationTurns)
			}
			if r.LifecycleBlockedSeconds == nil || *r.LifecycleBlockedSeconds != 3 {
				t.Fatalf("blocked = %#v", r.LifecycleBlockedSeconds)
			}
			if r.Boundaries.FinalizedLatencySeconds == nil || *r.Boundaries.FinalizedLatencySeconds != 20 {
				t.Fatalf("latency = %#v", r.Boundaries.FinalizedLatencySeconds)
			}
			if len(r.Incomplete) != 0 {
				t.Fatalf("incomplete = %v", r.Incomplete)
			}
		}},
		{name: "missing dispatcher transcript is null and incomplete", check: func(t *testing.T, r Report) {
			if r.Usage.Dispatcher != nil {
				t.Fatalf("dispatcher usage = %#v", r.Usage.Dispatcher)
			}
			if !strings.Contains(strings.Join(r.Incomplete, " "), "dispatcher_transcript") {
				t.Fatalf("incomplete = %v", r.Incomplete)
			}
		}},
		{name: "mismatched subagent name is rejected", dispatcher: true, mismatch: true, check: func(t *testing.T, r Report) {
			if r.Usage.Dispatcher != nil {
				t.Fatalf("dispatcher usage = %#v", r.Usage.Dispatcher)
			}
			if !strings.Contains(strings.Join(r.Incomplete, " "), "dispatcher_transcript") {
				t.Fatalf("incomplete = %v", r.Incomplete)
			}
		}},
		{name: "synthetic model is filtered", check: func(t *testing.T, r Report) {
			if strings.Contains(strings.Join(r.Models.Main, " "), "<synthetic>") {
				t.Fatalf("main models = %v", r.Models.Main)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, projects := fixture(t, tt.dispatcher, tt.mismatch)
			got := Build(state, projects, "job-1")
			tt.check(t, got)
			if _, err := json.Marshal(got); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPilotReportWarmDispatcherScopesEachJob(t *testing.T) {
	state, projects := fixture(t, true, false)
	path := filepath.Join(projects, "project-a", "session-1", "subagents", "agent-dispatcher-1.jsonl")
	dispatch := strings.Join([]string{
		`{"type":"assistant","requestId":"before","message":{"id":"before","role":"assistant","content":[],"usage":{"input_tokens":100}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"start-1","name":"mcp__codex__codex_start","input":{"jobId":"job-1","taskId":"task-1"}}],"usage":{"input_tokens":1}}}`,
		`{"type":"assistant","requestId":"job-1-request","message":{"id":"job-1-request","role":"assistant","content":[],"usage":{"input_tokens":7}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"start-2","name":"mcp__codex__codex_start","input":{"jobId":"job-2","taskId":"task-2"}}],"usage":{"input_tokens":1}}}`,
		`{"type":"assistant","requestId":"job-2-request","message":{"id":"job-2-request","role":"assistant","content":[],"usage":{"input_tokens":9}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(dispatch), 0600); err != nil {
		t.Fatal(err)
	}
	got := Build(state, projects, "job-1")
	if got.Usage.Dispatcher == nil || got.Usage.Dispatcher.Input == nil || *got.Usage.Dispatcher.Input != 8 {
		t.Fatalf("dispatcher usage = %#v, want current job only (8)", got.Usage.Dispatcher)
	}
	records, err := readLines(path)
	if err != nil {
		t.Fatal(err)
	}
	jobTwo, _ := analyzeDispatcher(records, "job-2", "task-2")
	if jobTwo == nil || jobTwo.Input == nil || *jobTwo.Input != 10 {
		t.Fatalf("second job dispatcher usage = %#v, want only second job (10)", jobTwo)
	}
}

func TestPilotReportMainDeduplicatesRequestID(t *testing.T) {
	state, projects := fixture(t, false, false)
	path := filepath.Join(projects, "project-a", "session-1.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(`{"type":"assistant","timestamp":"2026-10-09T12:00:01Z","requestId":"req-1","message":{"id":"msg-1-stream","role":"assistant","model":"claude-main","content":[{"type":"tool_use","id":"tool-stream","name":"mcp__codex-main__codex","input":{"jobId":"job-1","taskId":"task-1"}}],"usage":{"input_tokens":10,"output_tokens":4}}}` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got := Build(state, projects, "job-1")
	if got.Usage.Main == nil || got.Usage.Main.Input == nil || *got.Usage.Main.Input != 10 {
		t.Fatalf("main input = %#v, want duplicated request counted once (10)", got.Usage.Main)
	}
}

func TestPilotReportWorkerAndAcceptanceBoundaries(t *testing.T) {
	state, projects := fixture(t, false, false)
	got := Build(state, projects, "job-1")
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	boundaries := report["boundaries"].(map[string]any)
	if boundaries["worker_seconds"] != float64(10) || boundaries["to_acceptance_seconds"] != float64(15) {
		t.Fatalf("boundaries = %#v, want worker_seconds=10 and to_acceptance_seconds=15", boundaries)
	}
	jobPath := filepath.Join(state, "codex_exec_mcp", "jobs", "job-1.json")
	jobData, err := os.ReadFile(jobPath)
	if err != nil {
		t.Fatal(err)
	}
	var job map[string]any
	if err := json.Unmarshal(jobData, &job); err != nil {
		t.Fatal(err)
	}
	delete(job, "acceptance")
	jobData, err = json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobPath, jobData, 0600); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(state, "telemetry", "events", "by_task", "task-1.jsonl")
	eventData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	var withoutAcceptance []string
	for _, line := range strings.Split(strings.TrimSpace(string(eventData)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event_type"] != "CLAUDE_DECISION_RECORDED" {
			withoutAcceptance = append(withoutAcceptance, line)
		}
	}
	if err := os.WriteFile(eventsPath, []byte(strings.Join(withoutAcceptance, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got = Build(state, projects, "job-1")
	if got.Boundaries.ToAcceptanceSeconds != nil {
		t.Fatalf("to_acceptance_seconds = %#v, want null when acceptance is absent", got.Boundaries.ToAcceptanceSeconds)
	}
}

func TestPilotReportFindsDispatcherFromCodexStart(t *testing.T) {
	state, projects := fixture(t, false, false)
	eventsPath := filepath.Join(state, "telemetry", "events", "by_task", "task-1.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		delete(event, "agent_id")
		events = append(events, event)
	}
	var encoded strings.Builder
	for _, event := range events {
		b, _ := json.Marshal(event)
		encoded.Write(b)
		encoded.WriteByte('\n')
	}
	if err := os.WriteFile(eventsPath, []byte(encoded.String()), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projects, "project-a", "session-1", "subagents", "agent-found.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"mcp__codex__codex_start","input":{"jobId":"job-1"}}]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := Build(state, projects, "job-1")
	if got.IDs.DispatcherAgent == nil || *got.IDs.DispatcherAgent != "found" {
		t.Fatalf("dispatcher_agent_id = %#v, want found", got.IDs.DispatcherAgent)
	}
}

func TestPilotReportPromptResponses(t *testing.T) {
	state, projects := fixture(t, false, false)
	path := filepath.Join(projects, "project-a", "session-1.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"type":"user","timestamp":"2026-10-09T12:00:06Z","message":{"role":"user","content":"first prompt"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-10-09T12:00:06.500Z","message":{"role":"assistant","stop_reason":"end_turn","content":[]}}` + "\n" +
		`{"type":"user","timestamp":"2026-10-09T12:00:07Z","message":{"role":"user","content":[{"type":"text","text":"second prompt"}]}}` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := Build(state, projects, "job-1")
	if len(got.PromptResponseSeconds) != 1 || got.PromptResponseSeconds[0] != 0.5 {
		t.Fatalf("prompt responses = %v", got.PromptResponseSeconds)
	}
	if !strings.Contains(strings.Join(got.Incomplete, " "), "prompt_response_missing") {
		t.Fatalf("incomplete = %v", got.Incomplete)
	}
}

func TestPilotReportCosts(t *testing.T) {
	state, projects := fixture(t, true, false)
	pricesPath := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(pricesPath, []byte(`{"claude-main":{"input":1,"output":2,"cache_write_5m":3,"cache_write_1h":4,"cache_read":5}}`), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pricesPath)
	if err != nil {
		t.Fatal(err)
	}
	var prices map[string]Prices
	if err := json.Unmarshal(data, &prices); err != nil {
		t.Fatal(err)
	}
	got := BuildWithPrices(state, projects, "job-1", prices)
	if got.APIEquivalentUSD == nil || got.APIEquivalentUSD.Main == nil || *got.APIEquivalentUSD.Main != 0.000043 || got.APIEquivalentUSD.Dispatcher != nil {
		t.Fatalf("api equivalent = %#v", got.APIEquivalentUSD)
	}
	if !strings.Contains(strings.Join(got.Incomplete, " "), "dispatcher_price_missing") {
		t.Fatalf("incomplete = %v", got.Incomplete)
	}
}
