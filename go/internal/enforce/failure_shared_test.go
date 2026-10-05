package enforce

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"purser-hook/internal/ledger"
)

func TestFailureSharedParityFixture(t *testing.T) {
	data, err := os.ReadFile("../../../.claude/hooks/orchestrator/tests/fixtures/go_policy/failure.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) < 9 {
		t.Fatal("reply replay fixture missing")
	}
	c := fixture.Cases[2]
	input, pending := mapValue(c["event"]), mapValue(c["pending"])
	input["duration_ms"] = json.Number("1234")
	root, key := t.TempDir(), "reply_replay_failure"
	evidence := failureEvidence(input, pending)
	path, err := persistFailureEvidence(root, key, evidence)
	var want map[string]any
	if err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &want) != nil || !reflect.DeepEqual(want, c["evidence"]) {
		t.Fatalf("evidence=%#v", want)
	}
	wantSession := mapValue(c["session"])
	outcome := mapValue(wantSession["last_outcome"])
	outcome["evidence_path"] = path
	if err := saveFailureSession(root, input, pending, outcome); err != nil {
		t.Fatal(err)
	}
	task, project := stringValue(pending["task_id"]), stringValue(pending["project_key"])
	sessionPath, _ := ledger.PathForTask(filepath.Join(root, "projects", project), task)
	saved := ledger.Load(sessionPath, task)
	if saved.Status != ledger.OK || !reflect.DeepEqual(saved.Record, wantSession) {
		t.Fatalf("session=%#v", saved.Record)
	}
	failureObservation(root, input, pending)
	data, err = os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", task+".jsonl"))
	var observation map[string]any
	if err != nil || json.Unmarshal([]byte(strings.TrimSpace(string(data))), &observation) != nil {
		t.Fatalf("observation: %v", err)
	}
	payload := mapValue(observation["payload"])
	for k := range payload {
		if strings.HasSuffix(k, "_at") {
			delete(payload, k)
		}
	}
	if !reflect.DeepEqual(payload, c["observation_payload"]) {
		t.Fatalf("observation=%#v", payload)
	}
	byName := map[string]map[string]any{}
	for _, item := range fixture.Cases {
		byName[stringValue(item["name"])] = item
	}
	syncOutput := stringValue(byName["sync_failure"]["output"])
	syncPath := strings.SplitN(syncOutput, "and evidence is at ", 2)[1]
	syncPath = strings.TrimSuffix(syncPath, ".")
	if got := stringValue(mapValue(failureSuccessOutput(syncPath, true)["hookSpecificOutput"])["additionalContext"]); got != syncOutput {
		t.Fatalf("success output=%q want=%q", got, syncOutput)
	}
	note := byName["not_enforced_note"]
	wantOutput := stringValue(note["output"])
	wantPath := strings.SplitN(wantOutput, "and evidence is at ", 2)[1]
	wantPath = strings.TrimSuffix(wantPath, ". Routing was NOT enforced by PreToolUse.")
	wantPath = strings.TrimSuffix(wantPath, ".")
	ctx := mapValue(failureSuccessOutput(wantPath, false)["hookSpecificOutput"])
	if got := stringValue(ctx["additionalContext"]); got != wantOutput {
		t.Fatalf("bypass success output=%q want=%q", got, wantOutput)
	}
	for _, name := range []string{"lease_release_failure", "pending_removal_failure"} {
		want := stringValue(byName[name]["output"])
		ctx := mapValue(failureErrorOutput(strings.SplitN(want, "Evidence cleanup error: ", 2)[1]))["hookSpecificOutput"].(map[string]any)
		if ctx["additionalContext"] != want || ctx["hookEventName"] != "PostToolUseFailure" {
			t.Fatalf("%s error output=%#v want=%q", name, ctx, want)
		}
	}
	async := byName["async_job_record_removal"]
	root = t.TempDir()
	pendingPath := filepath.Join(root, "pending", "call.json")
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	jobID := strings.Repeat("a", 64)
	jobPath := filepath.Join(root, "async_jobs", jobID+".json")
	if err := os.MkdirAll(filepath.Dir(jobPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupFailureTail(root, pendingPath,
		map[string]any{"tool_name": "mcp__codex-main__codex_start"}, map[string]any{"job_id": jobID}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("async job record remains: %v (%v)", async["async_job_exists"], err)
	}
	if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending record remains: %v", err)
	}
	if stringValue(async["output"]) == "" {
		t.Fatal("async fixture output missing")
	}
	for _, name := range []string{"missing_pending", "malformed_pending"} {
		item := byName[name]
		input := mapValue(item["event"])
		root := t.TempDir()
		if name == "malformed_pending" {
			key, err := callKey(input)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "pending", key+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		got := PostToolUseFailure(input, root)
		gotContext := stringValue(mapValue(got["hookSpecificOutput"])["additionalContext"])
		if gotContext != stringValue(item["output"]) {
			t.Fatalf("%s output=%q want=%q", name, gotContext, item["output"])
		}
	}
	root = t.TempDir()
	pendingPath = filepath.Join(root, "pending", "call.json")
	if err := os.MkdirAll(filepath.Dir(pendingPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pendingPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupFailureTail(root, pendingPath, nil, map[string]any{"project_key": "p", "lease_owner": "owner"}, false); err == nil {
		t.Fatal("missing lease owner succeeded")
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending removed before lease release succeeded: %v", err)
	}
	if err := os.Remove(pendingPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pendingPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pendingPath, "child"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupFailureTail(root, pendingPath, nil, map[string]any{}, false); err == nil {
		t.Fatal("non-empty pending directory removal succeeded")
	}
}
