package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type eventFixture struct {
	Agent       string      `json:"agent"`
	Stdout      string      `json:"stdout"`
	Stderr      string      `json:"stderr"`
	ExitCode    int         `json:"exitCode"`
	Interrupted bool        `json:"interrupted"`
	Expected    EventResult `json:"expected"`
}

func TestEventFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_events")
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Python oracle fixtures found; run the oracle with PURSER_REGEN_GO_JOBS_EVENTS=1")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var fixture eventFixture
			if err := json.Unmarshal(data, &fixture); err != nil {
				t.Fatal(err)
			}
			stream := NewEventStream(fixture.Agent)
			stream.Write([]byte(fixture.Stdout))
			actual := stream.Result(fixture.Interrupted, fixture.ExitCode, fixture.Stderr)
			if !reflect.DeepEqual(actual, fixture.Expected) {
				t.Fatalf("got %#v, want %#v", actual, fixture.Expected)
			}
		})
	}
}

func TestEventStreamRecordsThreadBeforeTerminalFailure(t *testing.T) {
	stream := NewEventStream("codex")
	threadID := stream.Write([]byte(`{"type":"thread.started","thread_id":"thread-resume"}` + "\n"))
	if threadID != "thread-resume" {
		t.Fatalf("thread ID not available before process exit: %q", threadID)
	}
	stream.Write([]byte(`{"type":"turn.failed","error":{"message":"failed"}}`))
	result := stream.Result(false, 1, "")
	if result.ThreadID != "thread-resume" || result.Status != "error" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestTestEvidenceKeepsLatestRunnerRunsAndBoundsOutput(t *testing.T) {
	events := []map[string]any{
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "go test ./...", "exit_code": float64(1), "aggregated_output": "old"}},
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "echo hello", "exit_code": float64(0), "aggregated_output": "ignored"}},
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "echo \"go test\"", "exit_code": float64(0), "aggregated_output": "ignored"}},
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "grep 'npm test' file", "exit_code": float64(0), "aggregated_output": "ignored"}},
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "go test ./...", "exit_code": float64(0), "aggregated_output": strings.Repeat("x", 5000)}},
	}
	got := TestEvidence(events)
	if len(got) != 1 || got[0]["exit_code"] != 0 || got[0]["command"] != "go test ./..." {
		t.Fatalf("test evidence = %#v", got)
	}
	if tail := got[0]["output_tail"].(string); tail != strings.Repeat("x", 4096) {
		t.Fatalf("output tail = %q, want last 4096 bytes unchanged", tail)
	}
}

func TestTestEvidenceRecoversOutputFromUpdatedItem(t *testing.T) {
	events := []map[string]any{
		{"type": "item.updated", "item": map[string]any{"id": "run-1", "type": "command_execution"}, "output_delta": "--- FAIL: TestExample"},
		{"type": "item.completed", "item": map[string]any{"id": "run-1", "type": "command_execution", "command": "go test ./pkg", "exit_code": float64(1), "status": "completed", "aggregated_output": ""}},
	}
	evidence := TestEvidence(events)
	if len(evidence) != 1 || evidence[0]["output_tail"] != "--- FAIL: TestExample" {
		t.Fatalf("TestEvidence() = %#v, want output recovered from item.updated", evidence)
	}
}

func TestTestEvidenceMarksMissingOutput(t *testing.T) {
	events := []map[string]any{
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "go test ./pkg", "exit_code": float64(1), "status": "completed", "timed_out": true}},
	}
	evidence := TestEvidence(events)
	if len(evidence) != 1 || evidence[0]["output_tail"] != "[no output captured; item status: completed; timeout: true]" {
		t.Fatalf("TestEvidence() = %#v, want explicit missing-output marker", evidence)
	}
}

func TestTestRunnerCommandRecognizesWrappedCommands(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{`"C:\Program Files\PowerShell\7\pwsh.exe" -Command "$env:GOCACHE = C:\\cache; go test ./internal/jobs/ -run TestX"`, true},
		{`powershell -c "go test ./..."`, true},
		{`bash -lc 'cd go && go test ./...'`, true},
		{`/bin/sh -c 'VAR=x pytest tests/x.py -q'`, true},
		{`zsh -c 'npm test'`, true},
		{`C:\Windows\System32\cmd.exe /c "cargo test --lib"`, true},
		{`python -m pytest tests/x.py -q`, true},
		{`C:\Python\python3.exe -m pytest`, true},
		{`py -m pytest`, true},
		{`uv run pytest tests/x.py`, true},
		{`npm test`, true},
		{`pnpm test`, true},
		{`yarn test`, true},
		{`cargo test`, true},
		{`echo go test ./...`, false},
		{`git log`, false},
	}
	for _, tc := range tests {
		t.Run(tc.command, func(t *testing.T) {
			if got := testRunnerCommand(tc.command); got != tc.want {
				t.Fatalf("testRunnerCommand(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}

func TestTestEvidenceFailedSeesFailureOutsideOutputCap(t *testing.T) {
	events := []map[string]any{
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "go test ./failed", "exit_code": float64(1)}},
	}
	for i := 0; i < 6; i++ {
		events = append(events, map[string]any{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": "go test ./pkg" + strconv.Itoa(i), "exit_code": float64(0)}})
	}
	if evidence := TestEvidence(events); len(evidence) != 6 {
		t.Fatalf("capped evidence length = %d, want 6", len(evidence))
	}
	if !TestEvidenceFailed(events) {
		t.Fatal("capped-out failure was not retained")
	}
}

func TestTestEvidenceFailedUsesLatestRunPerTargetExitCode(t *testing.T) {
	tests := []struct {
		name           string
		runs           []map[string]any
		latestExitCode int
		failed         bool
	}{
		{
			name: "later pass clears earlier failure",
			runs: []map[string]any{
				{"command": "GOCACHE=/old go test ./pkg", "exit_code": float64(1), "aggregated_output": "cache denied"},
				{"command": "GOCACHE=/new go test ./pkg", "exit_code": float64(0), "aggregated_output": "ok"},
			},
			latestExitCode: 0,
			failed:         false,
		},
		{
			name: "later failure overrides earlier pass",
			runs: []map[string]any{
				{"command": "GOCACHE=/old go test ./pkg", "exit_code": float64(0), "aggregated_output": "ok"},
				{"command": "GOCACHE=/new go test ./pkg", "exit_code": float64(1), "aggregated_output": "failed"},
			},
			latestExitCode: 1,
			failed:         true,
		},
		{
			name: "traceback text does not override zero exit",
			runs: []map[string]any{
				{"command": "go test ./pkg", "exit_code": float64(0), "aggregated_output": "Traceback (most recent call last): setup note"},
			},
			failed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := make([]map[string]any, 0, len(tc.runs))
			for _, run := range tc.runs {
				events = append(events, map[string]any{"type": "item.completed", "item": map[string]any{
					"type": "command_execution", "command": run["command"], "exit_code": run["exit_code"], "aggregated_output": run["aggregated_output"],
				}})
			}
			evidence := TestEvidence(events)
			if len(evidence) != 1 || evidence[0]["exit_code"] != tc.latestExitCode {
				t.Fatalf("TestEvidence() = %#v, want one latest run with exit code %d", evidence, tc.latestExitCode)
			}
			if got := TestEvidenceFailed(events); got != tc.failed {
				t.Fatalf("TestEvidenceFailed() = %v, want %v", got, tc.failed)
			}
		})
	}
}

func TestOpenCodeToolErrorIsNoteBeforeSuccessfulCompletion(t *testing.T) {
	stream := NewEventStream("opencode")
	stream.Write([]byte("{" +
		`"type":"tool_use","sessionID":"oc-note","part":{"tool":"write","state":{"status":"error","error":"Permission denied"}}` +
		"}\n" +
		`{"type":"text","sessionID":"oc-note","part":{"text":"done"}}` + "\n" +
		`{"type":"step_finish","sessionID":"oc-note","part":{"reason":"stop"}}` + "\n"))
	result := stream.Result(false, 0, "")
	if result.Status != "success" || result.ThreadID != "oc-note" || result.Final != "done" {
		t.Fatalf("unexpected successful result: %#v", result)
	}
	if !reflect.DeepEqual(result.Notes, []string{"OpenCode tool error: write: Permission denied"}) {
		t.Fatalf("unexpected tool notes: %#v", result.Notes)
	}
}

func TestOpenCodeToolErrorDoesNotMaskProcessFailure(t *testing.T) {
	stream := NewEventStream("opencode")
	stream.Write([]byte(`{"type":"tool_use","sessionID":"oc-fail","part":{"tool":"write","state":{"status":"error","error":"Permission denied"}}}` + "\n"))
	result := stream.Result(false, 7, "worker failed")
	if result.Status != "error" || result.Failure != "worker failed" {
		t.Fatalf("unexpected process failure result: %#v", result)
	}
	if !reflect.DeepEqual(result.Notes, []string{"OpenCode tool error: write: Permission denied"}) {
		t.Fatalf("unexpected tool notes: %#v", result.Notes)
	}
}
