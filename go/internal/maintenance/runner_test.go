package maintenance

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeJobs struct {
	state       string
	jobID       string
	markerJobID string
	started     map[string]any
	results     []map[string]any
	resultCalls int
	cancel      map[string]any
	cancelCalls int
	onStart     func(string)
}

func (f *fakeJobs) Start(state string, args map[string]any) (map[string]any, error) {
	f.state = state
	f.started = args
	if f.jobID == "" {
		f.jobID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	if f.onStart != nil {
		f.onStart(state)
	}
	return map[string]any{"structuredContent": map[string]any{"jobId": f.jobID, "state": "RUNNING"}}, nil
}
func (f *fakeJobs) Result(_, _ string) map[string]any {
	markers, _ := filepath.Glob(filepath.Join(f.state, "projects", "*", "maintenance", "cleanup-active.json"))
	if len(markers) > 0 {
		if data, err := os.ReadFile(markers[0]); err == nil {
			var marker map[string]any
			_ = json.Unmarshal(data, &marker)
			f.markerJobID, _ = marker["job_id"].(string)
		}
	}
	index := f.resultCalls
	f.resultCalls++
	if index >= len(f.results) {
		index = len(f.results) - 1
	}
	if index < 0 {
		return map[string]any{"state": "RUNNING"}
	}
	return map[string]any{"structuredContent": f.results[index]}
}
func (f *fakeJobs) Cancel(_, _ string) map[string]any {
	f.cancelCalls++
	return map[string]any{"structuredContent": f.cancel}
}

func TestLifecycleOracle(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "maintenance_reports_oracle.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		LifecycleCases []struct {
			Name              string           `json:"name"`
			Results           []map[string]any `json:"results"`
			UserJob           bool             `json:"user_job"`
			CancelState       string           `json:"cancel_state"`
			WantError         string           `json:"want_error"`
			WantResultState   string           `json:"want_result_state"`
			WantCancelCalls   int              `json:"want_cancel_calls"`
			WantSlotRetained  bool             `json:"want_slot_retained"`
			WantActiveRemoved bool             `json:"want_active_removed"`
			WantMarkerJobID   bool             `json:"want_marker_job_id"`
		} `json:"lifecycle_cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if len(oracle.LifecycleCases) != 5 {
		t.Fatalf("expected five lifecycle cases, got %d", len(oracle.LifecycleCases))
	}
	for _, tc := range oracle.LifecycleCases {
		t.Run(tc.Name, func(t *testing.T) {
			state := t.TempDir()
			jobsDir := filepath.Join(state, "codex_exec_mcp", "jobs")
			fake := &fakeJobs{results: tc.Results, cancel: map[string]any{"state": tc.CancelState}}
			if tc.UserJob {
				fake.onStart = func(_ string) {
					_ = os.MkdirAll(jobsDir, 0700)
					_ = os.WriteFile(filepath.Join(jobsDir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.json"), []byte(`{"jobId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","state":"RUNNING"}`), 0600)
				}
			}
			current := time.Unix(100, 0)
			runner := Runner{Jobs: fake, Now: func() time.Time { return current }, Sleep: func(d time.Duration) { current = current.Add(d) }, PollInterval: time.Second, JobTimeout: 3 * time.Second}
			result, runErr := runner.Run(state, t.TempDir(), "cleanup", "oracle prompt")
			if tc.WantError != "" {
				if runErr == nil || runErr.Error() != tc.WantError {
					t.Fatalf("error = %v, want %q", runErr, tc.WantError)
				}
			} else if runErr != nil {
				t.Fatal(runErr)
			}
			if tc.WantResultState != "" && result["state"] != tc.WantResultState {
				t.Fatalf("result = %#v", result)
			}
			if fake.cancelCalls != tc.WantCancelCalls {
				t.Fatalf("cancel calls = %d", fake.cancelCalls)
			}
			slot := filepath.Join(state, "maintenance", "active.json")
			if got := fileExists(slot); got != tc.WantSlotRetained {
				t.Fatalf("slot exists = %v", got)
			}
			activeMarkers, _ := filepath.Glob(filepath.Join(state, "projects", "*", "maintenance", "cleanup-active.json"))
			if tc.WantActiveRemoved && len(activeMarkers) != 0 {
				t.Fatalf("active marker remains: %v", activeMarkers)
			}
			if tc.WantMarkerJobID && fake.resultCalls > 0 {
				// The marker is removed on exit; verify it held the durable ID during polling via hook below.
				if fake.markerJobID != fake.jobID {
					t.Fatalf("marker job id = %q", fake.markerJobID)
				}
			}
		})
	}
}

func fileExists(path string) bool { _, err := os.Stat(path); return !errors.Is(err, os.ErrNotExist) }
