package enforce

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"purser-hook/internal/contextio"
)

func TestCompletionReceiptTurkishLine(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	event := map[string]any{"session_id": "turkish-session", "tool_use_id": "turkish-call"}
	path := postReceiptStatePath(root, repo)
	state := readPostReceiptState(path, "turkish-session")
	state["last_context_language"] = "English"
	mustPostTest(t, writePostReceiptState(path, state))
	mustPostTest(t, os.WriteFile(filepath.Join(filepath.Dir(path), "purser-handoff.md"), []byte("## Goal\nUser language: Türkçe\n"), 0600))
	now := time.Now().UTC()
	quota := map[string]any{"5h_used_pct": 24, "7d_used_pct": 61, "snapshot_at": now.Format(time.RFC3339), "5h_resets_at": now.Add(29 * time.Minute).Format(time.RFC3339), "7d_resets_at": now.Add(134*time.Hour + time.Minute).Format(time.RFC3339)}
	receipt := queuePostCompletionReceipt(root, event, repo, map[string]any{"model": "model", "effort": "high", "codex_quota": quota})
	response := map[string]any{"completion_receipt": receipt, "systemMessage": "existing quota"}
	deliverPostCompletionReceipt(response, root)
	want := "existing quota\n⚓ Codex bitti: model (high) · 5s %24 kullanıldı, sıfırlanmaya 0s 28dk · 7g %61 kullanıldı, sıfırlanmaya 5g 14s"
	if response["systemMessage"] != want {
		t.Fatalf("Turkish receipt=%q want %q", response["systemMessage"], want)
	}
}

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
	state := readPostReceiptState(postReceiptStatePath(root, repo), "completion-session")
	if state["receipt_delivered"] == true || strings.Contains(postPendingGuidance(root, event, repo), "Codex done") {
		t.Fatal("queued completion marked delivered or added to Claude guidance")
	}
	deliverPostCompletionReceipt(cached, root)
	message := stringValue(cached["systemMessage"])
	want := "existing quota\n⚓ Codex done: model (high) · 5h 24% used, resets in 1h 19m · 7d 61% used, resets in 3d 1h"
	if message != want {
		t.Fatalf("receipt=%q want %q", message, want)
	}
	state = readPostReceiptState(postReceiptStatePath(root, repo), "completion-session")
	if state["receipt_delivered"] != true || mapValue(state["receipt_pending"])["7d_resets_at"] != quota["7d_resets_at"] {
		t.Fatal("durable delivery or quota discarded")
	}
	if _, err := os.Stat(filepath.Join(root, "receipt_deliveries", stringValue(receipt["id"])+".json")); err != nil {
		t.Fatalf("delivered without durable record: %v", err)
	}
	t.Setenv("PURSER_STATE_DIR", root)
	if guidance := string(contextio.UserPromptSubmit(map[string]any{"session_id": "completion-session", "cwd": repo, "prompt": "Continue."}, time.Now())); strings.Contains(guidance, "Codex done") || strings.Contains(guidance, "Write one line") {
		t.Fatalf("completion in UserPromptSubmit guidance: %s", guidance)
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

func TestCompletionReceiptFailedDeliveryWriteKeepsPending(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	event := map[string]any{"session_id": "failed-session", "tool_use_id": "failed-call"}
	receipt := queuePostCompletionReceipt(root, event, repo, map[string]any{"model": "model", "effort": "high"})
	// Force delivery record serialization to fail after the delivery lock is acquired.
	receipt["invalid"] = math.NaN()
	response := map[string]any{"completion_receipt": receipt}
	deliverPostCompletionReceipt(response, root)
	state := readPostReceiptState(postReceiptStatePath(root, repo), "failed-session")
	if response["systemMessage"] != nil || state["receipt_delivered"] == true || mapValue(state["receipt_pending"])["model"] != "model" {
		t.Fatalf("failed delivery suppressed pending receipt: %#v %#v", response, state)
	}
	delete(receipt, "invalid")
	response["completion_receipt"] = receipt
	deliverPostCompletionReceipt(response, root)
	if response["systemMessage"] != "⚓ Codex done: model (high)" || readPostReceiptState(postReceiptStatePath(root, repo), "failed-session")["receipt_delivered"] != true {
		t.Fatalf("pending receipt did not recover: %#v", response)
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

func TestCompletionReceiptEnglishFallback(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	key := projectKey(&repo)
	if err := contextio.SetGear(root, key, "up"); err != nil {
		t.Fatal(err)
	}
	event := map[string]any{"session_id": "receipt-gear-session", "tool_use_id": "completed-call"}
	bundle := map[string]any{"model": "configured-sol", "effort": "medium", "codex_quota": nil}
	for _, language := range []string{"", "French"} {
		t.Run(language, func(t *testing.T) {
			event["tool_use_id"] = "completed-call-" + language
			path := postReceiptStatePath(root, repo)
			state := readPostReceiptState(path, "receipt-gear-session")
			state["last_context_language"] = language
			mustPostTest(t, writePostReceiptState(path, state))
			response := map[string]any{"completion_receipt": queuePostCompletionReceipt(root, event, repo, bundle)}
			deliverPostCompletionReceipt(response, root)
			want := "⚓ Codex done: configured-sol (medium)"
			if response["systemMessage"] != want {
				t.Fatalf("receipt=%q want %q", response["systemMessage"], want)
			}
		})
	}
}
