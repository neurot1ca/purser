package contextio

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaintenanceOwnershipPythonOracle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "maintenance_reports_oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		OwnershipCases []struct {
			Name               string   `json:"name"`
			Acquired           bool     `json:"acquired"`
			Blocked            bool     `json:"blocked"`
			Fields             []string `json:"fields"`
			ExistsWhileHeld    bool     `json:"exists_while_held"`
			ExistsAfterRelease bool     `json:"exists_after_release"`
			StillExists        bool     `json:"still_exists"`
			StaleBefore        bool     `json:"stale_before"`
			Recovered          bool     `json:"recovered"`
			Stale              bool     `json:"stale"`
			Readable           bool     `json:"readable"`
			MarkerFields       []string `json:"marker_fields"`
			JobID              string   `json:"job_id"`
			JobState           string   `json:"job_state"`
		} `json:"ownership_cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{}{}
	for _, tc := range fixture.OwnershipCases {
		cases[tc.Name] = struct{}{}
	}
	for _, name := range []string{"free-slot", "held-slot", "stale-slot-recovery", "python-held-slot-seen-by-go", "go-held-slot-seen-by-python", "live-active-marker", "stale-active-marker", "live-job-active-marker", "stale-job-active-marker", "malformed-active-marker", "malformed-timestamp-active-marker"} {
		if _, ok := cases[name]; !ok {
			t.Fatalf("oracle case %q missing", name)
		}
	}
	get := func(name string) (tc struct {
		Name               string   `json:"name"`
		Acquired           bool     `json:"acquired"`
		Blocked            bool     `json:"blocked"`
		Fields             []string `json:"fields"`
		ExistsWhileHeld    bool     `json:"exists_while_held"`
		ExistsAfterRelease bool     `json:"exists_after_release"`
		StillExists        bool     `json:"still_exists"`
		StaleBefore        bool     `json:"stale_before"`
		Recovered          bool     `json:"recovered"`
		Stale              bool     `json:"stale"`
		Readable           bool     `json:"readable"`
		MarkerFields       []string `json:"marker_fields"`
		JobID              string   `json:"job_id"`
		JobState           string   `json:"job_state"`
	}) {
		for _, item := range fixture.OwnershipCases {
			if item.Name == name {
				return item
			}
		}
		t.Fatalf("missing %s", name)
		return
	}

	t.Run("free-slot", func(t *testing.T) {
		state := t.TempDir()
		release, err := AcquireMaintenanceSlot(state)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(state, "maintenance", "active.json")
		value, ok := ReadMaintenanceActive(path)
		if !ok {
			t.Fatal("slot marker unreadable")
		}
		if len(value) != len(get("free-slot").Fields) || value["pid"] == nil || value["started_at"] == nil {
			t.Fatalf("slot schema: %#v", value)
		}
		if !get("free-slot").Acquired || !get("free-slot").ExistsWhileHeld {
			t.Fatal("Python oracle did not capture acquired slot")
		}
		release()
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("slot remains after release: %v", err)
		}
		if get("free-slot").ExistsAfterRelease {
			t.Fatal("Python oracle expected release")
		}
	})
	t.Run("held-and-cross-language-slot", func(t *testing.T) {
		state := t.TempDir()
		path := filepath.Join(state, "maintenance", "active.json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		marker := map[string]any{"pid": os.Getpid(), "started_at": float64(time.Now().UnixNano()) / 1e9}
		writeMaintenanceFixture(t, path, marker)
		_, err := AcquireMaintenanceSlot(state)
		if !errors.Is(err, os.ErrExist) {
			t.Fatalf("Python-held slot not excluded: %v", err)
		}
		if !get("held-slot").Blocked || !get("held-slot").StillExists {
			t.Fatal("Python oracle did not capture held slot")
		}
		// A Go-created marker has the same JSON fields Python requires to reject it.
		goState := t.TempDir()
		release, err := AcquireMaintenanceSlot(goState)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		goMarker, ok := ReadMaintenanceActive(filepath.Join(goState, "maintenance", "active.json"))
		if !ok || goMarker["pid"] == nil || goMarker["started_at"] == nil {
			t.Fatalf("Go slot is not Python-readable: %#v", goMarker)
		}
		if !get("go-held-slot-seen-by-python").Blocked {
			t.Fatal("Python oracle expected Go-compatible slot to block")
		}
	})
	t.Run("stale-slot-recovery", func(t *testing.T) {
		state := t.TempDir()
		path := filepath.Join(state, "maintenance", "active.json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		old := float64(time.Now().Add(-maintenanceBackgroundLimit-time.Second).UnixNano()) / 1e9
		writeMaintenanceFixture(t, path, map[string]any{"pid": os.Getpid(), "started_at": old})
		if !maintenanceGlobalSlotStale(path, time.Now()) || !get("stale-slot-recovery").StaleBefore {
			t.Fatal("expired slot not stale per oracle")
		}
		release, err := AcquireMaintenanceSlot(state)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		value, ok := ReadMaintenanceActive(path)
		if !ok || value["started_at"].(json.Number).String() == "" || !get("stale-slot-recovery").Recovered {
			t.Fatal("stale slot was not replaced")
		}
	})
	t.Run("active-marker-cases", func(t *testing.T) {
		state := t.TempDir()
		activeDir := filepath.Join(state, "markers")
		if err := os.MkdirAll(activeDir, 0700); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		live := filepath.Join(activeDir, "live.json")
		if err := CreateMaintenanceActive(live, map[string]any{"started_at": now.Format("20060102T150405.000000Z")}); err != nil {
			t.Fatal(err)
		}
		if got := MaintenanceActiveStale(live, state, now); got != get("live-active-marker").Stale {
			t.Fatalf("live marker stale=%t", got)
		}
		stale := filepath.Join(activeDir, "stale.json")
		if err := CreateMaintenanceActive(stale, map[string]any{"started_at": "20000101T000000.000000Z"}); err != nil {
			t.Fatal(err)
		}
		if got := MaintenanceActiveStale(stale, state, now); got != get("stale-active-marker").Stale {
			t.Fatalf("stale marker stale=%t", got)
		}
		recoveredMarker := filepath.Join(activeDir, "recovered.json")
		if err := CreateMaintenanceActive(recoveredMarker, map[string]any{"started_at": "20000101T000000.000000Z"}); err != nil {
			t.Fatal(err)
		}
		if err := CreateOrRecoverMaintenanceActive(recoveredMarker, state, map[string]any{"project_root": "repo", "started_at": now.Format("20060102T150405.000000Z"), "state": "RUNNING"}, now); err != nil {
			t.Fatal(err)
		}
		recoveredValue, ok := ReadMaintenanceActive(recoveredMarker)
		if !ok || recoveredValue["started_at"] != now.Format("20060102T150405.000000Z") {
			t.Fatalf("stale marker recovery failed: %#v", recoveredValue)
		}
		jobID := get("live-job-active-marker").JobID
		jobs := filepath.Join(state, "codex_exec_mcp", "jobs")
		if err := os.MkdirAll(jobs, 0700); err != nil {
			t.Fatal(err)
		}
		jobPath := filepath.Join(jobs, jobID+".json")
		activeJob := filepath.Join(activeDir, "job.json")
		writeMaintenanceFixture(t, jobPath, map[string]any{"jobId": jobID, "state": "RUNNING"})
		if err := CreateMaintenanceActive(activeJob, map[string]any{"job_id": jobID}); err != nil {
			t.Fatal(err)
		}
		if got := MaintenanceActiveStale(activeJob, state, now); got != get("live-job-active-marker").Stale {
			t.Fatalf("live job stale=%t", got)
		}
		writeMaintenanceFixture(t, jobPath, map[string]any{"jobId": jobID, "state": "SUCCEEDED"})
		if got := MaintenanceActiveStale(activeJob, state, now); got != get("stale-job-active-marker").Stale {
			t.Fatalf("terminal job stale=%t", got)
		}
		malformed := filepath.Join(activeDir, "malformed.json")
		if err := os.WriteFile(malformed, []byte("{bad"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, ok := ReadMaintenanceActive(malformed); ok != get("malformed-active-marker").Readable {
			t.Fatalf("malformed read=%t", ok)
		}
		if got := MaintenanceActiveStale(malformed, state, now); got != get("malformed-active-marker").Stale {
			t.Fatalf("malformed marker stale=%t", got)
		}
		badTime := filepath.Join(activeDir, "bad-time.json")
		if err := CreateMaintenanceActive(badTime, map[string]any{"started_at": "bad"}); err != nil {
			t.Fatal(err)
		}
		if got := MaintenanceActiveStale(badTime, state, now); got != get("malformed-timestamp-active-marker").Stale {
			t.Fatalf("bad timestamp stale=%t", got)
		}
		if err := ClearMaintenanceActive(badTime); err != nil {
			t.Fatal(err)
		}
	})
}
