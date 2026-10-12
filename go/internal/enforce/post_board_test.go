package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPostBoardPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_board.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name  string            `json:"name"`
			Files map[string]string `json:"files"`
			Board *string           `json:"board"`
			IDs   []string          `json:"ids"`
		} `json:"cases"`
		Advice []struct {
			Receipt  map[string]any `json:"receipt"`
			Now      string         `json:"now"`
			Expected string         `json:"expected"`
		} `json:"advice"`
		Dispatch []struct {
			Pending  map[string]any `json:"pending"`
			ETA      *int           `json:"eta"`
			Expected string         `json:"expected"`
		} `json:"dispatch"`
		Guidance string `json:"guidance"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tc.Files {
				path := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			board, ids := PostProjectJobBoard(root, "fixture", now)
			if tc.Board == nil {
				if board != "" || len(ids) != 0 {
					t.Fatalf("board=%q ids=%v want hidden", board, ids)
				}
				return
			}
			if board != *tc.Board || strings.Join(ids, "|") != strings.Join(tc.IDs, "|") {
				t.Fatalf("board/ids mismatch\n got: %q %v\nwant: %q %v", board, ids, *tc.Board, tc.IDs)
			}
		})
	}
	for _, tc := range fixture.Advice {
		now, err := time.Parse(time.RFC3339, tc.Now)
		if err != nil {
			t.Fatal(err)
		}
		if got := PostReceiptAdvice(tc.Receipt, now); got != tc.Expected {
			t.Errorf("advice=%q want %q", got, tc.Expected)
		}
	}
	for _, tc := range fixture.Dispatch {
		if got := PostDispatchReceipt(tc.Pending, tc.ETA); got != tc.Expected {
			t.Errorf("dispatch=%q want %q", got, tc.Expected)
		}
	}
	if got := PostApplyGuidance("", "0123"); got != fixture.Guidance {
		t.Errorf("guidance=%q want %q", got, fixture.Guidance)
	}
}

func TestPostApplyGuidanceUsesModelContext(t *testing.T) {
	jobID := strings.Repeat("a", 64)
	quota := "⚓ Purser: Codex quota left: 5h 97% (resets 23:50) · 7d 76% (resets Oct 14)"
	response := map[string]any{
		"systemMessage": quota,
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "ExistingEvent",
			"additionalContext": "Existing model context",
		},
	}

	appendPostApplyGuidance(response, jobID)

	output := mapValue(response["hookSpecificOutput"])
	context := stringValue(output["additionalContext"])
	if want := "Existing model context\n" + PostApplyGuidance("", jobID); context != want {
		t.Fatalf("additionalContext=%q want %q", context, want)
	}
	if output["hookEventName"] != "ExistingEvent" {
		t.Fatalf("hookEventName=%v want existing value", output["hookEventName"])
	}
	message := stringValue(response["systemMessage"])
	if message != quota || strings.Contains(message, "**") || strings.Contains(message, jobID) {
		t.Fatalf("systemMessage=%q want plain quota only", message)
	}

	withoutOutput := map[string]any{}
	appendPostApplyGuidance(withoutOutput, jobID)
	missingOutput := mapValue(withoutOutput["hookSpecificOutput"])
	if missingOutput["hookEventName"] != "PostToolUse" || stringValue(missingOutput["additionalContext"]) != PostApplyGuidance("", jobID) {
		t.Fatalf("default hook output=%#v", missingOutput)
	}
}

func TestRefreshCachedBoardReplacesStaleSnapshot(t *testing.T) {
	response := map[string]any{"hookSpecificOutput": map[string]any{"additionalContext": "review summary\nPurser jobs:\n> | queued behind old-holder |\nlater guidance"}}
	refreshCachedBoard(response, "Purser jobs:\n> | current board |")
	context := stringValue(mapValue(response["hookSpecificOutput"])["additionalContext"])
	if strings.Contains(context, "old-holder") || !strings.Contains(context, "current board") ||
		!strings.Contains(context, "review summary") || !strings.Contains(context, "later guidance") {
		t.Fatalf("refreshed context = %q", context)
	}
}

func TestPostBoardTerminalAndQueuedStates(t *testing.T) {
	id := strings.Repeat("a", 64)
	tests := []struct {
		name   string
		job    map[string]any
		want   string
		unwant string
	}{
		{name: "cancelled", job: map[string]any{"state": "SUCCEEDED", "label": "Task cancelled", "discarded": true, "finishedAt": 1800000000, "boardDisplayCount": 0}, want: "| Task cancelled | — | discarded |", unwant: "fetching result"},
		{name: "failed", job: map[string]any{"state": "FAILED", "label": "Task failed", "finishedAt": 1800000000, "boardDisplayCount": 0}, want: "| Task failed | — | failed |", unwant: "coding"},
		{name: "applied while running", job: map[string]any{"state": "RUNNING", "label": "Task applied", "applyState": "applied"}, want: "| Task applied | — | done |", unwant: "coding"},
		{name: "rejected is done", job: map[string]any{"state": "SUCCEEDED", "label": "Task rejected", "applyState": "rejected", "finishedAt": 1800000000, "boardDisplayCount": 0}, want: "| Task rejected | — | done |", unwant: "in review"},
		{name: "superseded hides after usual count", job: map[string]any{"state": "SUCCEEDED", "label": "Task superseded", "applyState": "superseded", "finishedAt": 1800000000, "boardDisplayCount": 2}, want: ""},
		{name: "running coded", job: map[string]any{"state": "SUCCEEDED", "label": "Task coded", "finishedAt": 1800000000, "boardDisplayCount": 0}, want: "coded · fetching result"},
		{name: "queued without label", job: map[string]any{"state": "QUEUED", "taskId": "task-queued"}, want: "| task-queued | — | queued |"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			index := map[string]any{"project_key": "fixture", "jobId": id, "call": "call-1", "call_key": "key-1"}
			job := map[string]any{"jobId": id, "call": "call-1"}
			for key, value := range tc.job {
				job[key] = value
			}
			write := func(path string, value any) {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "async_jobs", "job.json"), index)
			write(filepath.Join(root, "codex_exec_mcp", "jobs", id+".json"), job)

			board, _ := PostProjectJobBoard(root, "fixture", time.Unix(1800000000, 0))
			if tc.want == "" {
				if board != "" {
					t.Fatalf("board=%q want hidden", board)
				}
				return
			}
			if board == "" || !strings.Contains(board, tc.want) {
				t.Fatalf("board=%q want it to contain %q", board, tc.want)
			}
			if tc.unwant != "" && strings.Contains(board, tc.unwant) {
				t.Fatalf("board=%q must not contain %q", board, tc.unwant)
			}
		})
	}
}
