package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
