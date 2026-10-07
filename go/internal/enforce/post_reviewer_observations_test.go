package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"purser-hook/internal/review"
)

func TestPostReviewerObservationsPythonFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_policy", "post_reviewer_observations.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name                string           `json:"name"`
			Started             bool             `json:"started"`
			SkippedReason       string           `json:"skipped_reason"`
			Provider            string           `json:"provider"`
			RequestedModel      string           `json:"requested_model"`
			Outcome             string           `json:"outcome"`
			Attributes          map[string]any   `json:"attributes"`
			DurationMS          float64          `json:"duration_ms"`
			InitialFindingCount int              `json:"initial_finding_count"`
			Expected            []map[string]any `json:"events"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, "fixture-repo")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			event := map[string]any{"session_id": "reviewer-fixture-session"}
			recordPostReviewerObservations(root, event, nil, "reviewer-task", "0123456789abcdef",
				test.Started, test.Provider, test.RequestedModel, test.SkippedReason,
				review.BridgeOutcome{Outcome: test.Outcome, Attributes: test.Attributes}, nil,
				time.Duration(test.DurationMS*float64(time.Millisecond)), test.InitialFindingCount)
			data, err := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "reviewer-task.jsonl"))
			if len(test.Expected) == 0 && os.IsNotExist(err) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var actual []map[string]any
			for _, line := range splitObservationLines(data) {
				var event map[string]any
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatal(err)
				}
				if event["event_type"] == "REVIEWER1_STARTED" || event["event_type"] == "REVIEWER1_FINISHED" || event["event_type"] == "REVIEWER1_FAILED" || event["event_type"] == "REVIEWER1_SKIPPED" {
					delete(event, "event_id")
					delete(event, "ts_utc")
					actual = append(actual, event)
				}
			}
			if !reflect.DeepEqual(actual, test.Expected) {
				t.Fatalf("reviewer observations = %#v, Python oracle %#v", actual, test.Expected)
			}
		})
	}
}

func TestPostReviewerUnavailableDiagnosticsStayInTelemetry(t *testing.T) {
	root := t.TempDir()
	diagnostics := map[string]any{"reviewer_stdout": "stdout evidence", "reviewer_stderr": "stderr evidence", "reviewer_exit_code": 7, "reviewer_parse_error": "bad json"}
	recordPostReviewerObservations(root, map[string]any{"session_id": "diagnostic-session"}, nil, "diagnostic-task", "0123456789abcdef",
		true, "agy", "model", "", review.BridgeOutcome{Outcome: "unavailable", Attributes: map[string]any{"result": "REVIEWER1_UNAVAILABLE", "review_status": "UNAVAILABLE"}, Diagnostics: diagnostics},
		nil, time.Second, -1)
	data, err := os.ReadFile(filepath.Join(root, "telemetry", "events", "by_task", "diagnostic-task.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var failed map[string]any
	for _, line := range splitObservationLines(data) {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event["event_type"] == "REVIEWER1_FAILED" {
			failed = event
		}
	}
	payload := mapValue(failed["payload"])
	if failed == nil || payload["reviewer_stdout"] != "stdout evidence" || payload["reviewer_stderr"] != "stderr evidence" || payload["reviewer_exit"] != "exit code 7" || payload["reviewer_parse_error"] != "bad json" {
		t.Fatalf("diagnostic telemetry = %#v", failed)
	}
	if _, leaked := review.Unavailable("REVIEWER_RESPONSE_INVALID", "bad", nil, "agy").Map()["reviewer_stdout"]; leaked {
		t.Fatal("diagnostics leaked into Claude-facing attributes")
	}
}

func splitObservationLines(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		index := 0
		for index < len(data) && data[index] != '\n' {
			index++
		}
		if index > 0 {
			lines = append(lines, data[:index])
		}
		if index == len(data) {
			break
		}
		data = data[index+1:]
	}
	return lines
}
