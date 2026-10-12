package enforce

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewIntervalRecordsDeduplicatedUsageAndUnknowns(t *testing.T) {
	t.Run("deduplicates request ids and separates cached input", func(t *testing.T) {
		root := t.TempDir()
		transcript := filepath.Join(root, "session.jsonl")
		data, err := os.ReadFile(filepath.Join("testdata", "task_usage_aggregate.json"))
		if err != nil {
			t.Fatal(err)
		}
		var fixture struct {
			Cases []struct {
				Records []map[string]any `json:"records"`
			} `json:"cases"`
		}
		if err := json.Unmarshal(data, &fixture); err != nil {
			t.Fatal(err)
		}
		records := fixture.Cases[0].Records
		for _, row := range records {
			row["timestamp"] = "2026-10-12T10:00:01Z"
		}
		records = append(records, records[0])
		outside := clonePostMap(records[0])
		outside["requestId"], outside["timestamp"] = "coding-request", "2026-10-12T09:59:59Z"
		records = append(records, outside)
		writeReviewIntervalTranscript(t, transcript, records)
		delivered := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
		terminal := delivered.Add(4 * time.Second)
		recordReviewIntervalDeliveryAt(root, map[string]any{"transcript_path": transcript}, "job-1", "task-1", delivered)
		recordReviewIntervalDecisionAt(root, "job-1", "accepted", terminal)
		recordReviewIntervalDecisionAt(root, "job-1", "send_back", terminal.Add(time.Second))
		got := readReviewInterval(t, root, "job-1")
		if got["decision"] != "accepted" || got["review_seconds"] != 4.0 || got["usage_status"] != "KNOWN" {
			t.Fatalf("interval terminal fields = %#v", got)
		}
		if got["input_tokens"] != float64(700) || got["cached_input_tokens"] != float64(115) || got["cache_creation_input_tokens"] != float64(70) || got["output_tokens"] != float64(240) {
			t.Fatalf("interval usage = %#v", got)
		}
		ids, _ := got["request_ids"].([]any)
		if len(ids) != 2 || ids[0] != "req-1" || ids[1] != "req-2" {
			t.Fatalf("request ids = %#v", ids)
		}
	})

	t.Run("marks missing request usage unknown", func(t *testing.T) {
		root := t.TempDir()
		transcript := filepath.Join(root, "session.jsonl")
		writeReviewIntervalTranscript(t, transcript, []map[string]any{
			{"type": "assistant", "timestamp": "2026-10-12T10:00:01Z", "message": map[string]any{"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}},
		})
		delivered := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
		recordReviewIntervalDeliveryAt(root, map[string]any{"transcript_path": transcript}, "job-2", "task-2", delivered)
		recordReviewIntervalDecisionAt(root, "job-2", "send_back", delivered.Add(2*time.Second))
		got := readReviewInterval(t, root, "job-2")
		if got["decision"] != "send_back" || got["usage_status"] != "UNKNOWN" || got["usage_unknown_reason"] != "MISSING_REVIEW_USAGE" || got["input_tokens"] != nil {
			t.Fatalf("unknown interval = %#v", got)
		}
	})
}

func writeReviewIntervalTranscript(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	var data []byte
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readReviewInterval(t *testing.T, root, jobID string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "telemetry", "review_intervals", jobID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
