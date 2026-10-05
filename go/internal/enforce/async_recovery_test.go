package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"purser-hook/internal/ledger"
)

func TestAsyncFinalizingRecoveryPythonFixture(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "async_recovery.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Oracle string                    `json:"oracle"`
		Cases  map[string]map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Oracle != "codex._async_post_followup FINALIZING recovery helpers" || len(fixture.Cases) != 5 {
		t.Fatalf("unexpected Python oracle: %#v", fixture)
	}

	for name, expected := range fixture.Cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			event := map[string]any{"session_id": "recovery-session", "tool_use_id": "recovery-call"}
			key, err := callKey(event)
			if err != nil {
				t.Fatal(err)
			}
			project, task := "project-fixture", "recovery-task"
			pending := map[string]any{"task_id": task, "project_key": project}
			index := map[string]any{"call_key": key, "project_key": project, "stage": "FINALIZING"}
			artifact := filepath.Join(root, "bundle_artifacts", key+".json")
			raw := filepath.Join(root, "raw_tool_outputs", key+".json")
			if name != "missing_artifact" {
				if err := os.MkdirAll(filepath.Dir(raw), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(raw, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name != "missing_artifact" {
				if err := os.MkdirAll(filepath.Dir(artifact), 0700); err != nil {
					t.Fatal(err)
				}
				if name == "corrupt_artifact" {
					if err := os.WriteFile(artifact, []byte("{broken"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					bundle := baseBundle("")
					bundle["task_id"], bundle["raw_output_path"], bundle["claude_review_mode"] = task, raw, "AUDIT"
					bundle["audit_receipt"], bundle["implementer_report"] = map[string]any{"ok": true}, "Done."
					encoded, _ := canonicalJSON(bundle)
					if err := os.WriteFile(artifact, encoded, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "ledger_recorded" {
				thread := "thread-recovery"
				record := ledger.BuildSessionRecord(task, &thread, "normal", "model", "high", map[string]any{
					"last_outcome": map[string]any{"raw_output_path": raw},
				})
				if saved := ledger.Save(filepath.Join(root, "projects", project), record); saved.Status != ledger.OK {
					t.Fatalf("seed ledger: %#v", saved)
				}
			}
			response, handled, err := recoverAsyncFinalizing(root, event, index, pending, 8000, 1200)
			if name == "corrupt_artifact" {
				if err == nil || handled {
					t.Fatalf("corrupt artifact result = %#v, %v, want error", response, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantRecovered := name != "missing_artifact"
			if handled != wantRecovered || expected["recovered"] != nil && expected["recovered"] != handled {
				t.Fatalf("handled=%v, expected fixture %#v", handled, expected)
			}
			if !handled {
				return
			}
			if index["stage"] != "CONSUMED" || response == nil {
				t.Fatalf("recovery did not consume index: %#v %#v", index, response)
			}
			persisted, artifactErr := readAsyncObject(artifact)
			if artifactErr != nil {
				t.Fatal(artifactErr)
			}
			ledgerRejected := false
			if findings, ok := persisted["findings"].([]any); ok {
				for _, rawFinding := range findings {
					if mapValue(rawFinding)["code"] == "LEDGER_RECORD_REJECTED" {
						ledgerRejected = true
					}
				}
			}
			if want, ok := expected["ledger_rejected"].(bool); ok && ledgerRejected != want {
				t.Fatalf("ledger rejection=%v; fixture expects %v", ledgerRejected, want)
			}
			stored, readErr := readAsyncObject(filepath.Join(root, "async_jobs", key+".json"))
			if readErr != nil || !reflect.DeepEqual(stored["response"], response) {
				t.Fatalf("durable response = %#v, %v; want %#v", stored, readErr, response)
			}
			if name == "rejected_response" {
				persisted, readErr := readAsyncObject(artifact)
				if readErr != nil || persisted["claude_review_mode"] != "FULL" {
					t.Fatalf("rejection artifact = %#v, %v", persisted, readErr)
				}
				found := false
				for _, raw := range persisted["findings"].([]any) {
					if mapValue(raw)["code"] == "LEDGER_RECORD_REJECTED" {
						found = true
					}
				}
				if !found || expected["artifact_has_rejection"] != true {
					t.Fatal("rejection was not persisted to artifact")
				}
				replay, replayed, replayErr := recoverAsyncFinalizing(root, event, index, pending, 8000, 1200)
				if replayErr != nil || !replayed || !reflect.DeepEqual(replay, response) {
					t.Fatalf("replay = %#v, %v, %v", replay, replayed, replayErr)
				}
			}
		})
	}
}
