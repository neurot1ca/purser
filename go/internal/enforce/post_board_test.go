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
