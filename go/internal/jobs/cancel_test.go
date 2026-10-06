package jobs

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"purser-hook/internal/lease"
)

func TestCancelMatchesPythonOracle(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_cancel", "cancel.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name        string         `json:"name"`
			JobID       string         `json:"jobId"`
			Record      map[string]any `json:"record"`
			Result      map[string]any `json:"result"`
			Files       map[string]any `json:"files"`
			SideEffects []any          `json:"sideEffects"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := scratch(t)
			dir := filepath.Join(root, "codex_exec_mcp", "jobs")
			if tc.Record != nil {
				encoded, _ := json.Marshal(tc.Record)
				record, decodeErr := Decode(encoded)
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				if err = Write(filepath.Join(dir, tc.JobID+".json"), record); err != nil {
					t.Fatal(err)
				}
			}
			var host *SignalHost
			if tc.Name == "running_ack" {
				terminal := make(chan struct{})
				host, err = StartSignalHost(tc.JobID, "secret", terminal, func() bool {
					current, readErr := Read(filepath.Join(dir, tc.JobID+".json"))
					if readErr != nil {
						return false
					}
					finishCancelled(current)
					if Write(filepath.Join(dir, tc.JobID+".json"), current) != nil {
						return false
					}
					close(terminal)
					return true
				})
				if err != nil {
					t.Fatal(err)
				}
				defer host.Close()
				tcp := host.Addr().(*net.TCPAddr)
				current, _ := Read(filepath.Join(dir, tc.JobID+".json"))
				_ = current.Set("signalPort", tcp.Port)
				if err = Write(filepath.Join(dir, tc.JobID+".json"), current); err != nil {
					t.Fatal(err)
				}
			}
			if tc.Name == "terminal" {
				seedCancelLease(t, root, tc.JobID)
			}
			sideEffects := []any{}
			options := CancelOptions{
				RecordDecision: func(job map[string]any, decision, kind, source string) {
					sideEffects = append(sideEffects, map[string]any{"call": "recordDecision", "jobId": job["jobId"],
						"decision": decision, "kind": kind, "sourceJobId": source})
				},
				RecordScopeConflictOutcome: func(job map[string]any, action string) {
					sideEffects = append(sideEffects, map[string]any{"call": "recordScopeConflictOutcome", "jobId": job["jobId"], "action": action})
				},
				ReleaseScopeLease: func(stateRoot, jobID string) bool {
					sideEffects = append(sideEffects, map[string]any{"call": "releaseScopeLease", "jobId": jobID})
					return lease.ReleaseJobScopeLease(stateRoot, jobID)
				},
			}
			got := Cancel(root, map[string]any{"jobId": tc.JobID}, options)
			if !reflect.DeepEqual(got["structuredContent"], tc.Result["structuredContent"]) ||
				got["isError"] != tc.Result["isError"] {
				t.Fatalf("result mismatch\n got: %#v\nwant: %#v", got, tc.Result)
			}
			if !reflect.DeepEqual(sideEffects, tc.SideEffects) {
				t.Fatalf("side effects mismatch\n got: %#v\nwant: %#v", sideEffects, tc.SideEffects)
			}
			if tc.Name == "terminal" {
				assertCancelLeaseReleased(t, root)
			}
			files := map[string]any{}
			if record, readErr := Read(filepath.Join(dir, tc.JobID+".json")); readErr == nil {
				files[tc.JobID+".json"] = cleanCancelRecord(recordMap(record))
			}
			wantFiles := tc.Files
			if wantFiles == nil {
				wantFiles = map[string]any{}
			}
			if !reflect.DeepEqual(files, wantFiles) {
				t.Fatalf("records mismatch\n got: %#v\nwant: %#v", files, wantFiles)
			}
		})
	}
}

func TestCancelWaitsForDelayedTerminalWrite(t *testing.T) {
	root := scratch(t)
	jobID, token := "0123456789abcdef0123456789abcdef", "secret"
	dir := filepath.Join(root, "codex_exec_mcp", "jobs")
	path := filepath.Join(dir, jobID+".json")
	record, err := Decode([]byte(`{"jobId":"` + jobID + `","state":"RUNNING","signalToken":"` + token + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = Write(path, record); err != nil {
		t.Fatal(err)
	}
	terminal := make(chan struct{})
	written := make(chan struct{})
	host, err := StartSignalHost(jobID, token, terminal, func() bool {
		go func() {
			time.Sleep(200 * time.Millisecond)
			current, readErr := Read(path)
			if readErr == nil {
				finishCancelled(current)
				if Write(path, current) == nil {
					close(terminal)
					close(written)
				}
			}
		}()
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	current, _ := Read(path)
	_ = current.Set("signalPort", host.Addr().(*net.TCPAddr).Port)
	if err = Write(path, current); err != nil {
		t.Fatal(err)
	}

	got := Cancel(root, map[string]any{"jobId": jobID})
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("fake host did not write terminal record")
	}
	value, _ := got["structuredContent"].(map[string]any)
	if value["state"] != "CANCELLED" {
		t.Fatalf("cancel result state = %v, want CANCELLED (result %#v)", value["state"], got)
	}
}

func seedCancelLease(t *testing.T, root, jobID string) {
	t.Helper()
	project, owner := "0123456789abcdef", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	state := filepath.Join(root, "projects", project)
	store := lease.New(state)
	if got := store.Acquire(owner, "task-terminal", []string{"src"}, lease.AcquireOptions{JobID: &jobID, TimeoutSeconds: 60}, 100); got.Status != "OK" {
		t.Fatalf("seed lease: %#v", got)
	}
	index, _ := json.Marshal(map[string]any{"jobId": jobID, "project_key": project, "call_key": owner})
	if err := os.MkdirAll(filepath.Join(root, "async_jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "async_jobs", jobID+".json"), index, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertCancelLeaseReleased(t *testing.T, root string) {
	t.Helper()
	path := filepath.Join(root, "projects", "0123456789abcdef", "scope_leases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	owners := value["owners"].(map[string]any)
	owner := owners["ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"].(map[string]any)
	if owner["state"] != "RELEASED" {
		t.Fatalf("scope lease not released: %#v", owner)
	}
}

func cleanCancelRecord(record map[string]any) map[string]any {
	for _, key := range []string{"finishedAt", "discardedAt"} {
		if _, exists := record[key]; exists {
			record[key] = "TIME"
		}
	}
	if _, exists := record["signalPort"]; exists {
		record["signalPort"] = "PORT"
	}
	return record
}
