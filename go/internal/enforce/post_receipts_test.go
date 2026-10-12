package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestCompletionReceiptDurableSnapshotAndSingleUIDelivery(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	event := map[string]any{"session_id": "completion-session", "tool_use_id": "completion-call"}
	now := time.Now().UTC()
	quota := map[string]any{"5h_used_pct": float64(24), "7d_used_pct": float64(61), "snapshot_at": now.Format(time.RFC3339), "5h_resets_at": now.Add(80 * time.Minute).Format(time.RFC3339), "7d_resets_at": now.Add(74 * time.Hour).Format(time.RFC3339)}
	receipt := queuePostCompletionReceipt(root, event, repo, map[string]any{"model": "model", "effort": "high", "codex_quota": quota})
	cached := map[string]any{"completion_receipt": receipt, "systemMessage": "existing quota", "hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "additionalContext": "FULL review"}}
	encoded, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	deliverPostCompletionReceipt(cached, root)
	message := stringValue(cached["systemMessage"])
	for _, field := range []string{"existing quota", "> ⚓ **Codex done:", "model model", "effort high", "5h used 24%", "7d used 61%", "time until 5h reset", "time until 7d reset"} {
		if !strings.Contains(message, field) {
			t.Fatalf("receipt omitted %q: %s", field, message)
		}
	}
	state := readPostReceiptState(postReceiptStatePath(root, repo), "completion-session")
	if mapValue(state["receipt_pending"])["7d_resets_at"] != quota["7d_resets_at"] {
		t.Fatal("durable quota discarded")
	}
	var replay map[string]any
	if err := json.Unmarshal(encoded, &replay); err != nil {
		t.Fatal(err)
	}
	deliverPostCompletionReceipt(replay, root)
	if replay["systemMessage"] != "existing quota" || !reflect.DeepEqual(replay["hookSpecificOutput"], cached["hookSpecificOutput"]) {
		t.Fatalf("cached receipt repeated or enforcement changed: %#v", replay)
	}
	if strings.Contains(postPendingGuidance(root, event, repo), "Codex done") {
		t.Fatal("completion requires a feedback turn")
	}
}

func TestPostReceiptsPythonFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/post_receipts.json")
	mustPostTest(t, err)
	var fixture struct {
		Cases []struct {
			Name       string         `json:"name"`
			Repository string         `json:"repository"`
			Initial    map[string]any `json:"initial"`
			Bundle     map[string]any `json:"bundle"`
			Expected   map[string]any `json:"expected"`
		} `json:"cases"`
	}
	mustPostTest(t, json.Unmarshal(data, &fixture))
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root := t.TempDir()
			event := map[string]any{"session_id": "post-receipts-session"}
			path := postReceiptStatePath(root, test.Repository)
			switch test.Name {
			case "queue":
				queuePostCompletionReceipt(root, event, test.Repository, test.Bundle)
				queuePostReceipt(root, event, test.Repository, "> ⚓ **Codex: first**")
				queuePostReceipt(root, event, test.Repository, "> ⚓ **Codex: second**")
				state := readPostReceiptState(path, event["session_id"].(string))
				pending := mapValue(state["dispatch_pending"])
				if !reflect.DeepEqual(pending["lines"], test.Expected["lines"]) || !reflect.DeepEqual(state["receipt_pending"], test.Expected["receipt"]) || postPendingGuidance(root, event, test.Repository) != test.Expected["guidance"] {
					t.Fatalf("queued receipt mismatch: %#v", pending)
				}
			case "delivered":
				mustPostTest(t, atomicWrite(path, mustJSON(test.Initial)))
				markPostGuidanceDelivered(root, event, test.Repository)
				first := readPostReceiptState(path, event["session_id"].(string))
				written, readErr := os.ReadFile(path)
				mustPostTest(t, readErr)
				markPostGuidanceDelivered(root, event, test.Repository)
				second := readPostReceiptState(path, event["session_id"].(string))
				repeated, readErr := os.ReadFile(path)
				mustPostTest(t, readErr)
				if !reflect.DeepEqual(written, repeated) || mapValue(first["version_pending"])["delivered"] != test.Expected["delivered_once"] || mapValue(second["version_pending"])["delivered"] != test.Expected["delivered_repeat"] {
					t.Fatalf("delivery marking mismatch: %#v", second["version_pending"])
				}
			case "guidance":
				mustPostTest(t, atomicWrite(path, mustJSON(test.Initial)))
				got := postPendingGuidance(root, event, test.Repository)
				if got != test.Expected["guidance"] {
					t.Fatalf("guidance=%q want %q", got, test.Expected["guidance"])
				}
			case "corrupt":
				mustPostTest(t, os.MkdirAll(filepath.Dir(path), 0700))
				mustPostTest(t, os.WriteFile(path, []byte("{broken"), 0600))
				if got := postPendingGuidance(root, event, test.Repository); got != test.Expected["guidance"] {
					t.Fatalf("corrupt-state guidance=%q", got)
				}
			case "absent":
				if got := postPendingGuidance(root, event, test.Repository); got != test.Expected["guidance"] {
					t.Fatalf("absent-state guidance=%q", got)
				}
			}
		})
	}
}

func TestPostDispatchReceiptDoesNotMarkLifecycleDelivered(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	event := map[string]any{"session_id": "receipt-session", "tool_use_id": "receipt-call"}
	statePath := postReceiptStatePath(root, repo)
	state := readPostReceiptState(statePath, "receipt-session")
	state["version_pending"] = map[string]any{"version": "1", "delivered": false}
	writePostReceiptState(statePath, state)
	pending := map[string]any{"label": "task", "model": "gpt-6-sol", "effort": "medium", "route": "normal"}
	response := appendPostDispatchReceipt(map[string]any{}, root, event, pending, repo)
	if response["hookSpecificOutput"] == nil || mapValue(readPostReceiptState(statePath, "receipt-session")["version_pending"])["delivered"] != false {
		t.Fatalf("receipt incorrectly marked lifecycle guidance delivered: %#v", response)
	}
}

func TestCompletionReceiptShowsEnforcedPairAndGearIndicator(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	key := projectKey(&repo)
	if err := contextio.SetGear(root, key, "up"); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"session_id": "receipt-gear-session", "tool_use_id": "completed-call"}
	bundle := map[string]any{"model": "configured-sol", "effort": "medium", "codex_quota": nil}
	response := map[string]any{"completion_receipt": queuePostCompletionReceipt(root, event, repo, bundle)}
	deliverPostCompletionReceipt(response, root)
	guidance := stringValue(response["systemMessage"])
	want := "Write one line in the user's language as `> ⚓ **Codex done: ...**`, showing model configured-sol, effort medium, gear up."
	if guidance != want {
		t.Fatalf("receipt guidance %q want %q", guidance, want)
	}
}
