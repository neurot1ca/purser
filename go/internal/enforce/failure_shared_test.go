package enforce

import (
	"encoding/json"
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
	if len(fixture.Cases) < 3 {
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
}
