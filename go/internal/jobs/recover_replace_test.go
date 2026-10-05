package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecoverReplacementPythonOracle(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_recover_replace", "oracle.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name         string           `json:"name"`
			Job          map[string]any   `json:"job"`
			Launch       map[string]any   `json:"launch"`
			Index        map[string]any   `json:"index"`
			Pending      map[string]any   `json:"pending"`
			Lease        map[string]any   `json:"lease"`
			LaunchError  string           `json:"launchError"`
			Result       string           `json:"result"`
			Record       map[string]any   `json:"record"`
			LaunchRecord map[string]any   `json:"launchRecord"`
			ThreadRecord map[string]any   `json:"threadRecord"`
			LeaseRecord  map[string]any   `json:"leaseRecord"`
			Launches     []map[string]any `json:"launches"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := scratch(t)
			id := c.Job["jobId"].(string)
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			writeJSON := func(path string, value any) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				b, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeJSON(filepath.Join(dir, id+".json"), c.Job)
			if c.Launch != nil {
				writeJSON(filepath.Join(dir, id+".launch"), c.Launch)
			}
			if c.Index != nil {
				writeJSON(filepath.Join(root, "async_jobs", id+".json"), c.Index)
			}
			if c.Pending != nil {
				writeJSON(filepath.Join(root, "pending", "owner-1.json"), c.Pending)
			}
			if c.Lease != nil {
				writeJSON(filepath.Join(root, "projects", "project-1", "scope_leases.json"), c.Lease)
			}
			launches := []map[string]any{}
			got := RecoverJob(root, id, 2_000_000_000, RecoverOptions{
				ProcessAlive: func(int, int64) string { return "DEAD" },
				GroupStopped: func(int) bool { return true },
				NewToken:     func() string { return strings.Repeat("a1", 16) },
				Clock:        func() float64 { return 2_000_000_000 },
				LaunchHost: func(_ string, _ string, generation int64, token string) error {
					launches = append(launches, map[string]any{"generation": generation, "token": token})
					if c.LaunchError != "" {
						return errors.New(c.LaunchError)
					}
					return nil
				},
			})
			if got != c.Result {
				t.Fatalf("result = %s, want %s", got, c.Result)
			}
			actual := recordMap(readJob(dir, id))
			if c.Name == "launch_missing" {
				actual["message"] = "Recovery launch data unavailable: <OS_ERROR>"
			} else if c.Name == "ownership_missing" {
				actual["message"] = "Async ownership unavailable: <OS_ERROR>"
			}
			if !reflect.DeepEqual(actual, c.Record) {
				t.Fatalf("record mismatch:\n got %#v\nwant %#v", actual, c.Record)
			}
			launchData, err := os.ReadFile(filepath.Join(dir, id+".launch"))
			var launchRecord map[string]any
			if err == nil {
				_ = json.Unmarshal(launchData, &launchRecord)
			}
			if !reflect.DeepEqual(launchRecord, c.LaunchRecord) {
				t.Fatalf("launch mismatch: got %#v want %#v", launchRecord, c.LaunchRecord)
			}
			threadPath := RecordPath(filepath.Join(root, "codex_exec_mcp", "threads"), "thread-1")
			threadData, err := os.ReadFile(threadPath)
			var threadRecord map[string]any
			if err == nil {
				_ = json.Unmarshal(threadData, &threadRecord)
			}
			if !reflect.DeepEqual(threadRecord, c.ThreadRecord) {
				t.Fatalf("thread mismatch: got %#v want %#v", threadRecord, c.ThreadRecord)
			}
			var leaseRecord map[string]any
			if c.Lease != nil {
				value, err := os.ReadFile(filepath.Join(root, "projects", "project-1", "scope_leases.json"))
				if err != nil {
					t.Fatal(err)
				}
				_ = json.Unmarshal(value, &leaseRecord)
			}
			if c.LeaseRecord != nil {
				wantOwners := c.LeaseRecord["owners"].(map[string]any)
				gotOwners, _ := leaseRecord["owners"].(map[string]any)
				if !reflect.DeepEqual(gotOwners, wantOwners) {
					t.Fatalf("lease owners mismatch: got %#v want %#v", gotOwners, wantOwners)
				}
			}
			if !sameRecoveryJSON(launches, c.Launches) {
				t.Fatalf("launch calls: got %#v want %#v", launches, c.Launches)
			}
		})
	}
}

func sameRecoveryJSON(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}
